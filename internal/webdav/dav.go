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

	ms := multistatus{
		XMLNSD:  "DAV:",
		XMLNSOC: "http://owncloud.org/ns",
		XMLNSNC: "http://nextcloud.org/ns",
	}

	ctx := r.Context()

	// M2 — ánh xạ hai chiều: cây thư mục trả cho app được dựng từ logical_path
	// trong DB, KHÔNG đọc cây thư mục vật lý (file thật nằm ở photos/<yyyymm>/).

	// (1) rel trỏ tới một FILE đã upload? → trả metadata thật của file đó.
	if ph, err := h.pg.GetByLogicalPath(ctx, rel); err == nil && ph != nil {
		ms.Responses = append(ms.Responses, h.responseForPhoto(r.URL.Path, ph))
		writeMultistatus(w, h.logger, ms)
		return
	}

	// (2) rel là một THƯ MỤC: trả chính nó + (nếu Depth≥1) các con trực tiếp.
	ms.Responses = append(ms.Responses, dirResponse(r.URL.Path))

	if depth != "0" {
		prefix := strings.TrimSuffix(rel, "/") + "/"
		children, err := h.pg.ListByLogicalPrefix(ctx, prefix, 10000)
		if err != nil {
			h.logger.Warn("PROPFIND liệt kê con lỗi", "rel", rel, "err", err)
		}
		for _, c := range immediateChildren(rel, children) {
			childURL := path.Join(r.URL.Path, c.name)
			if c.isDir {
				childURL += "/"
				ms.Responses = append(ms.Responses, dirResponse(childURL))
			} else {
				ms.Responses = append(ms.Responses, h.responseForPhoto(childURL, c.photo))
			}
		}
	}

	writeMultistatus(w, h.logger, ms)
}

// childEntry là một mục con trực tiếp của thư mục logic đang PROPFIND.
type childEntry struct {
	name  string
	isDir bool
	photo *store.Photo // chỉ có khi isDir=false
}

// immediateChildren rút các con TRỰC TIẾP của thư mục logic dir từ danh sách ảnh
// có logical_path bắt đầu bằng "dir/". Mỗi logical_path "dir/<seg>/..." với phần
// còn lại nhiều segment → sinh một thư mục con "<seg>" (gộp trùng); nếu phần còn
// lại đúng một segment → là file "<seg>".
func immediateChildren(dir string, photos []store.Photo) []childEntry {
	dir = strings.TrimSuffix(dir, "/")
	prefix := dir + "/"
	seenDir := map[string]bool{}
	var out []childEntry
	for i := range photos {
		lp := photos[i].LogicalPath
		if !strings.HasPrefix(lp, prefix) {
			continue
		}
		rest := strings.TrimPrefix(lp, prefix)
		if rest == "" {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			// Còn nhiều segment → con là một thư mục trung gian.
			seg := rest[:idx]
			if seg != "" && !seenDir[seg] {
				seenDir[seg] = true
				out = append(out, childEntry{name: seg, isDir: true})
			}
		} else {
			// Đúng một segment → là file.
			out = append(out, childEntry{name: rest, isDir: false, photo: &photos[i]})
		}
	}
	return out
}

// writeMultistatus phát 207 Multi-Status kèm body XML.
func writeMultistatus(w http.ResponseWriter, logger *slog.Logger, ms multistatus) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus) // 207
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	if err := enc.Encode(ms); err != nil {
		logger.Error("encode PROPFIND lỗi", "err", err)
	}
}

// dirResponse tạo một entry collection (thư mục) cho PROPFIND. Thư mục trong
// không gian files/<user>/... là ẢO — dựng từ logical_path trong DB, không cần
// thư mục vật lý. Nếu DB chưa có gì khớp, đây vẫn là collection rỗng hợp lệ (M1)
// để app chấp nhận thư mục đích trước khi upload.
func dirResponse(href string) davResponse {
	return davResponse{
		Href: href,
		Propstat: davPropstat{
			Prop: davProp{
				ResourceType: &resourceType{Collection: &struct{}{}},
				LastModified: time.Now().UTC().Format(http.TimeFormat),
				ETag:         fmt.Sprintf("%q", "dir"),
			},
			Status: "HTTP/1.1 200 OK",
		},
	}
}

// responseForPhoto dựng davResponse cho một file từ bản ghi DB (không stat đĩa).
func (h *Handler) responseForPhoto(href string, ph *store.Photo) davResponse {
	size := ph.Size
	return davResponse{
		Href: href,
		Propstat: davPropstat{
			Prop: davProp{
				ResourceType:  &resourceType{},
				ContentLength: &size,
				ContentType:   ph.MIME,
				LastModified:  ph.TakenAt.UTC().Format(http.TimeFormat),
				ETag:          fmt.Sprintf("%q", etagOfHash(ph.SHA256)),
			},
			Status: "HTTP/1.1 200 OK",
		},
	}
}

// etagOfHash tạo ETag ổn định từ sha256 (16 ký tự đầu là đủ phân biệt).
func etagOfHash(sha string) string {
	if len(sha) >= 16 {
		return sha[:16]
	}
	return sha
}

// ---- MKCOL ----

func (h *Handler) mkcol(w http.ResponseWriter, r *http.Request, rel string) {
	// M1 — WebDAV ảo: app tạo thư mục đích (vd me/, me/InstantUpload) trước khi
	// upload, nhưng file thật đi vào photos/<yyyymm>/ nên KHÔNG cần tạo thư mục
	// vật lý tương ứng (tránh rác thư mục me/ trên đĩa). Luôn báo 201 thành công.
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

	res, _, err := h.storeWithDedup(r.Context(), staged, path.Base(rel), mtime, rel)
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
func (h *Handler) storeWithDedup(ctx context.Context, staged *storage.StageResult, baseName string, mtime time.Time, logicalPath string) (*storage.Result, bool, error) {
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
		h.indexPhoto(ctx, res, mtime, true, &dupOf, logicalPath)
		h.logger.Warn("phát hiện trùng → đưa vào thùng rác", "hash", staged.SHA256[:12], "rel", res.RelPath, "gốc", existing.RelPath)
		return res, true, nil
	}

	res, ferr := h.store.FinalizeStaged(staged.TempRel, "photos", baseName, staged.SHA256, mtime)
	if ferr != nil {
		_ = h.store.RemoveStaged(staged.TempRel)
		return nil, false, ferr
	}
	h.indexPhoto(ctx, res, mtime, false, nil, logicalPath)
	return res, false, nil
}

// ---- MOVE (đổi tên / di chuyển trong files/) ----

func (h *Handler) move(w http.ResponseWriter, r *http.Request, rel string) {
	dest := destRelFromHeader(r.Header.Get("Destination"))
	if dest == "" {
		http.Error(w, "thiếu Destination", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// M2: MOVE theo đường dẫn logic. File vật lý nằm ở photos/<yyyymm>/ theo ngày
	// gốc, ĐỘC LẬP với tên/thư mục logic, nên đổi tên/di chuyển trong không gian
	// app CHỈ cần cập nhật ánh xạ logic (logical_path + original_name) — không
	// động tới file trên đĩa. Giữ nguyên nguyên tắc file-là-nguồn-chân-lý.
	if ph, err := h.pg.GetByLogicalPath(ctx, rel); err == nil && ph != nil {
		if uerr := h.pg.UpdateLogicalPath(ctx, ph.RelPath, dest, path.Base(dest)); uerr != nil {
			h.logger.Warn("MOVE cập nhật ánh xạ logic lỗi", "rel", rel, "dest", dest, "err", uerr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent) // 204
		return
	}

	http.Error(w, "Not Found", http.StatusNotFound)
}

// ---- DELETE ----

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, rel string) {
	ctx := r.Context()
	// M2: DELETE theo đường dẫn logic. Tra DB → xoá file vật lý + bản ghi.
	if ph, err := h.pg.GetByLogicalPath(ctx, rel); err == nil && ph != nil {
		if rerr := h.store.Remove(ph.RelPath); rerr != nil {
			h.logger.Warn("xoá file vật lý lỗi", "rel", ph.RelPath, "err", rerr)
		}
		if derr := h.pg.DeletePhotoByRelPath(ctx, ph.RelPath); derr != nil {
			h.logger.Warn("xoá bản ghi lỗi", "rel", ph.RelPath, "err", derr)
		}
		w.WriteHeader(http.StatusNoContent) // 204
		return
	}

	// Fallback: không có ánh xạ logic → thử xoá theo đường dẫn vật lý như cũ.
	if err := h.store.Remove(rel); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent) // 204
}

// ---- GET / HEAD ----

func (h *Handler) get(w http.ResponseWriter, r *http.Request, rel string) {
	// M2: ánh xạ logic → vật lý. Tra DB theo logical_path để tìm file vật lý thật
	// (photos/<yyyymm>/...), vì đường dẫn app dùng khác đường dẫn trên đĩa.
	physRel := rel
	if ph, err := h.pg.GetByLogicalPath(r.Context(), rel); err == nil && ph != nil {
		physRel = ph.RelPath
	}

	fi, _, err := h.store.Stat(physRel)
	if err != nil || fi.IsDir() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	f, err := h.store.Open(physRel)
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
// logicalPath là đường dẫn app nhìn thấy (M2), ví dụ "me/Photos/IMG_0001.jpg".
func (h *Handler) indexPhoto(ctx context.Context, res *storage.Result, mtime time.Time, isTrash bool, dupOf *int64, logicalPath string) {
	taken := mtime
	if taken.IsZero() {
		taken = time.Now()
	}
	// original_name lấy từ tên trong đường dẫn logic (tên app đặt), không phải
	// tên vật lý (có thể bị thêm hậu tố hash chống trùng).
	origName := path.Base(res.RelPath)
	if logicalPath != "" {
		origName = path.Base(logicalPath)
	}
	ph := store.Photo{
		RelPath:      res.RelPath,
		OriginalName: origName,
		Size:         res.Size,
		SHA256:       res.SHA256,
		TakenAt:      taken,
		MIME:         contentTypeByExt(res.RelPath),
		IsTrash:      isTrash,
		DupOf:        dupOf,
		LogicalPath:  logicalPath,
	}
	if err := h.pg.UpsertPhoto(ctx, ph); err != nil {
		h.logger.Warn("index Postgres thất bại", "rel", res.RelPath, "err", err)
	}
}
