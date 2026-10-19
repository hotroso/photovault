// Package webdav cài WebDAV đủ để app Nextcloud auto-upload ảnh:
// PROPFIND, MKCOL, PUT, MOVE, DELETE trên /remote.php/dav/files/<user>/...
// và chunked upload v2 trên /remote.php/dav/uploads/<user>/... (file chunk.go).
package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/storage"
	"github.com/local/photovault/internal/store"
)

// Handler phục vụ các method WebDAV.
type Handler struct {
	cfg     *config.Config
	store   *storage.Storage
	pg      *store.Postgres
	logger  *slog.Logger
	filesNS string // tiền tố "/remote.php/dav/files/<user>"
}

// NewHandler tạo WebDAV handler.
func NewHandler(cfg *config.Config, st *storage.Storage, pg *store.Postgres, logger *slog.Logger) *Handler {
	return &Handler{
		cfg:     cfg,
		store:   st,
		pg:      pg,
		logger:  logger,
		filesNS: "/remote.php/dav/files/" + cfg.User,
	}
}

// ServeFiles điều phối các method trên không gian files/.
func (h *Handler) ServeFiles(w http.ResponseWriter, r *http.Request) {
	rel := h.relFromFiles(r.URL.Path)
	switch r.Method {
	case "PROPFIND":
		h.propfind(w, r, rel)
	case "MKCOL":
		h.mkcol(w, r, rel)
	case "PUT":
		h.put(w, r, rel)
	case "MOVE":
		h.move(w, r, rel)
	case "DELETE":
		h.delete(w, r, rel)
	case "HEAD", "GET":
		h.get(w, r, rel)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// relFromFiles tách phần đường dẫn tương đối sau /remote.php/dav/files/<user>.
// Kết quả luôn có tiền tố "<user>/" để lưu dưới DataDir/<user>/...
func (h *Handler) relFromFiles(urlPath string) string {
	p := strings.TrimPrefix(urlPath, "/remote.php/dav/files/")
	p = strings.Trim(p, "/")
	if p == "" {
		return h.cfg.User
	}
	// p đã bắt đầu bằng "<user>/..." (hoặc chỉ "<user>")
	return p
}

// ---- PROPFIND ----

type multistatus struct {
	XMLName   xml.Name     `xml:"d:multistatus"`
	XMLNSD    string       `xml:"xmlns:d,attr"`
	XMLNSOC   string       `xml:"xmlns:oc,attr"`
	XMLNSNC   string       `xml:"xmlns:nc,attr"`
	Responses []davResponse `xml:"d:response"`
}

type davResponse struct {
	Href     string     `xml:"d:href"`
	Propstat davPropstat `xml:"d:propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"d:prop"`
	Status string  `xml:"d:status"`
}

type davProp struct {
	ResourceType  *resourceType `xml:"d:resourcetype"`
	ContentLength *int64        `xml:"d:getcontentlength,omitempty"`
	ContentType   string        `xml:"d:getcontenttype,omitempty"`
	LastModified  string        `xml:"d:getlastmodified,omitempty"`
	ETag          string        `xml:"d:getetag,omitempty"`
}

type resourceType struct {
	Collection *struct{} `xml:"d:collection"`
}

func (h *Handler) propfind(w http.ResponseWriter, r *http.Request, rel string) {
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1"
	}

	fi, abs, err := h.store.Stat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		h.logger.Error("PROPFIND stat lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ms := multistatus{
		XMLNSD:  "DAV:",
		XMLNSOC: "http://owncloud.org/ns",
		XMLNSNC: "http://nextcloud.org/ns",
	}
	ms.Responses = append(ms.Responses, h.responseFor(r.URL.Path, fi))

	// Depth 1 trên thư mục: liệt kê các con.
	if fi.IsDir() && depth != "0" {
		entries, err := os.ReadDir(abs)
		if err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".part") {
					continue // bỏ qua file đang ghi dở
				}
				cfi, cerr := e.Info()
				if cerr != nil {
					continue
				}
				childURL := path.Join(r.URL.Path, e.Name())
				if cfi.IsDir() {
					childURL += "/"
				}
				ms.Responses = append(ms.Responses, h.responseFor(childURL, cfi))
			}
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus) // 207
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	if err := enc.Encode(ms); err != nil {
		h.logger.Error("encode PROPFIND lỗi", "err", err)
	}
}

func (h *Handler) responseFor(href string, fi os.FileInfo) davResponse {
	prop := davProp{
		LastModified: fi.ModTime().UTC().Format(http.TimeFormat),
		ETag:         fmt.Sprintf("%q", etagOf(fi)),
	}
	if fi.IsDir() {
		prop.ResourceType = &resourceType{Collection: &struct{}{}}
	} else {
		prop.ResourceType = &resourceType{}
		size := fi.Size()
		prop.ContentLength = &size
		prop.ContentType = contentTypeByExt(fi.Name())
	}
	return davResponse{
		Href: href,
		Propstat: davPropstat{
			Prop:   prop,
			Status: "HTTP/1.1 200 OK",
		},
	}
}

func etagOf(fi os.FileInfo) string {
	return fmt.Sprintf("%x-%x", fi.ModTime().UnixNano(), fi.Size())
}

// ---- MKCOL ----

func (h *Handler) mkcol(w http.ResponseWriter, r *http.Request, rel string) {
	if err := h.store.Mkdir(rel); err != nil {
		h.logger.Error("MKCOL lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated) // 201
}

// ---- PUT (ảnh nhỏ, upload trực tiếp) ----

func (h *Handler) put(w http.ResponseWriter, r *http.Request, rel string) {
	mtime := parseOCMtime(r.Header.Get("X-OC-Mtime"))
	expected := int64(-1)
	if r.ContentLength > 0 {
		expected = r.ContentLength
	}

	// Stage trước (hash một lần) rồi quyết định photos/ hay recyclebin/ theo dedup.
	staged, err := h.store.StageTemp(r.Body, expected)
	if err != nil {
		h.logger.Error("PUT stage lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	res, _, err := h.storeWithDedup(r.Context(), staged, path.Base(rel), mtime)
	if err != nil {
		// storeWithDedup đã RemoveStaged ở nhánh lỗi finalize.
		h.logger.Error("PUT lưu lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("OC-ETag", fmt.Sprintf("%q", res.SHA256[:16]))
	w.Header().Set("ETag", fmt.Sprintf("%q", res.SHA256[:16]))
	w.WriteHeader(http.StatusCreated) // 201
	h.logger.Info("đã lưu ảnh", "rel", res.RelPath, "size", res.Size, "sha256", res.SHA256[:12])
}

// storeWithDedup hoàn tất một file đã stage: tra hash toàn cục để quyết định
// đưa vào photos/ (bản gốc) hay recyclebin/ (bản trùng, is_trash=true, dup_of=gốc).
// Luôn trả về *Result khi không lỗi; dup=true nếu là bản trùng. Dù dedup hay
// không, một lỗi DB tra cứu KHÔNG bao giờ chặn backup — khi đó coi như không trùng.
func (h *Handler) storeWithDedup(ctx context.Context, staged *storage.StageResult, baseName string, mtime time.Time) (*storage.Result, bool, error) {
	existing, err := h.pg.GetBySHA256(ctx, staged.SHA256)
	if err != nil {
		h.logger.Warn("tra cứu hash thất bại, coi như không trùng", "hash", staged.SHA256[:12], "err", err)
		existing = nil
	}

	if existing != nil {
		res, ferr := h.store.FinalizeStaged(staged.TempRel, "recyclebin", baseName, staged.SHA256, mtime)
		if ferr != nil {
			_ = h.store.RemoveStaged(staged.TempRel)
			return nil, false, ferr
		}
		dupOf := existing.ID
		h.indexPhoto(ctx, res, mtime, true, &dupOf)
		h.logger.Warn("phát hiện trùng → đưa vào thùng rác", "hash", staged.SHA256[:12], "rel", res.RelPath, "gốc", existing.RelPath)
		return res, true, nil
	}

	res, ferr := h.store.FinalizeStaged(staged.TempRel, "photos", baseName, staged.SHA256, mtime)
	if ferr != nil {
		_ = h.store.RemoveStaged(staged.TempRel)
		return nil, false, ferr
	}
	h.indexPhoto(ctx, res, mtime, false, nil)
	return res, false, nil
}

// ---- MOVE (đổi tên / di chuyển trong files/) ----

func (h *Handler) move(w http.ResponseWriter, r *http.Request, rel string) {
	dest := destRelFromHeader(r.Header.Get("Destination"))
	if dest == "" {
		http.Error(w, "thiếu Destination", http.StatusBadRequest)
		return
	}
	if _, _, err := h.store.Stat(rel); err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	// Dùng rename qua storage: mở nguồn, ghi đích atomic rồi xoá nguồn.
	f, err := h.store.Open(rel)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	res, err := h.store.WriteAtomic(dest, f, -1, time.Time{})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_ = h.store.Remove(rel)
	h.indexPhoto(r.Context(), res, time.Time{}, false, nil)
	w.WriteHeader(http.StatusNoContent) // 204
}

// ---- DELETE ----

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, rel string) {
	if err := h.store.Remove(rel); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent) // 204
}

// ---- GET / HEAD ----

func (h *Handler) get(w http.ResponseWriter, r *http.Request, rel string) {
	fi, _, err := h.store.Stat(rel)
	if err != nil || fi.IsDir() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	f, err := h.store.Open(rel)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentTypeByExt(fi.Name()))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if r.Method == "HEAD" {
		return
	}
	_, _ = io.Copy(w, f)
}

// indexPhoto ghi bản ghi vào Postgres (no-op nếu PG chưa cấu hình).
// isTrash/dupOf đánh dấu bản trùng đã bị đưa vào thùng rác.
func (h *Handler) indexPhoto(ctx context.Context, res *storage.Result, mtime time.Time, isTrash bool, dupOf *int64) {
	taken := mtime
	if taken.IsZero() {
		taken = time.Now()
	}
	ph := store.Photo{
		RelPath:      res.RelPath,
		OriginalName: path.Base(res.RelPath),
		Size:         res.Size,
		SHA256:       res.SHA256,
		TakenAt:      taken,
		MIME:         contentTypeByExt(res.RelPath),
		IsTrash:      isTrash,
		DupOf:        dupOf,
	}
	if err := h.pg.UpsertPhoto(ctx, ph); err != nil {
		h.logger.Warn("index Postgres thất bại", "rel", res.RelPath, "err", err)
	}
}
