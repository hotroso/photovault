// Package webdav cài WebDAV đủ để app Nextcloud auto-upload ảnh:
// PROPFIND, MKCOL, PUT, MOVE, DELETE trên /remote.php/dav/files/<user>/...
// và chunked upload v2 trên /remote.php/dav/uploads/<user>/... (file chunk.go).
package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
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

	// dedupMu tuần tự hoá bước "tra hash → đặt file → ghi index". Không khoá thì
	// hai file giống hệt upload song song (app chạy tới 5 luồng) cùng thấy hash
	// chưa tồn tại và cả hai vào photos/. Bước stream dữ liệu (StageTemp) nằm
	// NGOÀI khoá nên upload vẫn song song; phần trong khoá chỉ vài mili-giây.
	dedupMu sync.Mutex

	// hasPreview cho biết có tạo được preview cho một MIME không (do dashboard
	// cung cấp, vì bộ sinh thumbnail nằm ở đó). nil = không có preview.
	hasPreview func(mime string) bool
}

// SetPreviewSupport gắn hàm kiểm tra preview để PROPFIND/SEARCH báo nc:has-preview.
func (h *Handler) SetPreviewSupport(f func(mime string) bool) { h.hasPreview = f }

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
	case "REPORT":
		// App dùng REPORT oc:filter-files để lấy danh sách yêu thích. Server không
		// có tính năng yêu thích → trả danh sách rỗng thay vì 405.
		writeMultistatus(w, h.logger, newMultistatus())
	case "PROPPATCH":
		// App đặt thuộc tính (yêu thích, live photo...). Không lưu, nhưng báo thành
		// công để app không coi là lỗi và thử lại.
		ms := newMultistatus()
		ms.Responses = append(ms.Responses, davResponse{
			Href:     r.URL.Path,
			Propstat: davPropstat{Status: "HTTP/1.1 200 OK"},
		})
		writeMultistatus(w, h.logger, ms)
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
	XMLName   xml.Name      `xml:"d:multistatus"`
	XMLNSD    string        `xml:"xmlns:d,attr"`
	XMLNSOC   string        `xml:"xmlns:oc,attr"`
	XMLNSNC   string        `xml:"xmlns:nc,attr"`
	Responses []davResponse `xml:"d:response"`
}

type davResponse struct {
	Href     string      `xml:"d:href"`
	Propstat davPropstat `xml:"d:propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"d:prop"`
	Status string  `xml:"d:status"`
}

// davProp là các thuộc tính trả cho app. Tên kèm tiền tố (d:/oc:/nc:) ghi
// nguyên văn vì app iOS (SwiftyXMLParser) tra cứu đúng các tên có tiền tố này.
type davProp struct {
	DisplayName   string        `xml:"d:displayname,omitempty"`
	ResourceType  *resourceType `xml:"d:resourcetype"`
	ContentLength *int64        `xml:"d:getcontentlength,omitempty"`
	ContentType   string        `xml:"d:getcontenttype,omitempty"`
	LastModified  string        `xml:"d:getlastmodified,omitempty"`
	ETag          string        `xml:"d:getetag,omitempty"`
	// oc:id là khoá chính của file trong DB của app; oc:fileid dùng để gọi
	// /index.php/core/preview?fileId=. Thiếu hai trường này app không xem được ảnh.
	ID          string `xml:"oc:id,omitempty"`
	FileID      string `xml:"oc:fileid,omitempty"`
	Permissions string `xml:"oc:permissions,omitempty"`
	Size        *int64 `xml:"oc:size,omitempty"`
	HasPreview  string `xml:"nc:has-preview,omitempty"`
	UploadTime  int64  `xml:"nc:upload_time,omitempty"`
}

// instanceID ghép sau fileid để tạo oc:id (giống Nextcloud: "%08d" + instanceid).
const instanceID = "ocpvault1"

// dirFileIDBase tách không gian id của thư mục ảo khỏi id ảnh (id ảnh là
// identity của Postgres, nhỏ hơn nhiều).
const dirFileIDBase = 2_000_000_000

func ocID(fileID int64) string { return fmt.Sprintf("%08d%s", fileID, instanceID) }

// dirFileID tạo id ổn định cho một thư mục ảo từ đường dẫn logic.
func dirFileID(logical string) int64 {
	f := fnv.New32a()
	_, _ = f.Write([]byte(strings.Trim(logical, "/")))
	return dirFileIDBase + int64(f.Sum32()%1_000_000_000)
}

// davHref tạo href WebDAV cho một đường dẫn logic ("me/Photos/a b.jpg"), mã
// hoá từng đoạn. Thư mục kết thúc bằng "/" (app nhận diện thư mục gốc bằng
// href "/remote.php/dav/files/<user>/").
func davHref(logical string, dir bool) string {
	segs := strings.Split(strings.Trim(logical, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	h := "/remote.php/dav/files/" + strings.Join(segs, "/")
	if dir {
		h += "/"
	}
	return h
}

func newMultistatus() multistatus {
	return multistatus{
		XMLNSD:  "DAV:",
		XMLNSOC: "http://owncloud.org/ns",
		XMLNSNC: "http://nextcloud.org/ns",
	}
}

type resourceType struct {
	Collection *struct{} `xml:"d:collection"`
}

func (h *Handler) propfind(w http.ResponseWriter, r *http.Request, rel string) {
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1"
	}

	ms := newMultistatus()
	ctx := r.Context()
	rel = strings.Trim(rel, "/")

	// M2 — ánh xạ hai chiều: cây thư mục trả cho app được dựng từ logical_path
	// trong DB, KHÔNG đọc cây thư mục vật lý (file thật nằm ở photos/<yyyymm>/).

	// (1) rel trỏ tới một FILE đã upload? → trả metadata thật của file đó.
	if ph, err := h.pg.GetByLogicalPath(ctx, rel); err == nil && ph != nil {
		ms.Responses = append(ms.Responses, h.responseForPhoto(davHref(rel, false), ph))
		writeMultistatus(w, h.logger, ms)
		return
	}

	// (2) rel là một THƯ MỤC: trả chính nó + (nếu Depth≥1) các con trực tiếp.
	// Danh sách hậu duệ dùng cả để tính ETag thư mục: ETag đổi khi có ảnh mới,
	// nhờ đó app biết cần tải lại danh sách.
	descendants, err := h.pg.ListByLogicalPrefix(ctx, rel+"/", 50000)
	if err != nil {
		h.logger.Warn("PROPFIND liệt kê con lỗi", "rel", rel, "err", err)
	}
	selfTag, subTags := dirETags(rel, descendants)
	ms.Responses = append(ms.Responses, dirResponse(rel, selfTag))

	if depth != "0" {
		for _, c := range immediateChildren(rel, descendants) {
			child := rel + "/" + c.name
			if c.isDir {
				ms.Responses = append(ms.Responses, dirResponse(child, subTags[c.name]))
			} else {
				ms.Responses = append(ms.Responses, h.responseForPhoto(davHref(child, false), c.photo))
			}
		}
	}

	writeMultistatus(w, h.logger, ms)
}

// dirETags tính ETag cho thư mục dir và cho từng thư mục con trực tiếp, từ
// danh sách hậu duệ (logical_path + sha256). Thêm/xoá/đổi tên ảnh → ETag đổi.
func dirETags(dir string, photos []store.Photo) (string, map[string]string) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	self := fnv.New64a()
	subs := map[string]hash.Hash64{}
	for _, p := range photos {
		rest, ok := strings.CutPrefix(p.LogicalPath, prefix)
		if !ok || rest == "" {
			continue
		}
		line := p.LogicalPath + "|" + p.SHA256 + "\n"
		_, _ = self.Write([]byte(line))
		if seg, _, isDir := strings.Cut(rest, "/"); isDir && seg != "" {
			hs, ok := subs[seg]
			if !ok {
				hs = fnv.New64a()
				subs[seg] = hs
			}
			_, _ = hs.Write([]byte(line))
		}
	}
	out := make(map[string]string, len(subs))
	for seg, hs := range subs {
		out[seg] = fmt.Sprintf("%016x", hs.Sum64())
	}
	return fmt.Sprintf("%016x", self.Sum64()), out
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
func dirResponse(logical, etag string) davResponse {
	var zero int64
	id := dirFileID(logical)
	return davResponse{
		Href: davHref(logical, true),
		Propstat: davPropstat{
			Prop: davProp{
				DisplayName:  path.Base(logical),
				ResourceType: &resourceType{Collection: &struct{}{}},
				LastModified: time.Now().UTC().Format(http.TimeFormat),
				ETag:         fmt.Sprintf("%q", etag),
				ID:           ocID(id),
				FileID:       strconv.FormatInt(id, 10),
				Permissions:  "RGDNVCK",
				Size:         &zero,
			},
			Status: "HTTP/1.1 200 OK",
		},
	}
}

// responseForPhoto dựng davResponse cho một file từ bản ghi DB (không stat đĩa).
func (h *Handler) responseForPhoto(href string, ph *store.Photo) davResponse {
	size := ph.Size
	name := ph.OriginalName
	if ph.LogicalPath != "" {
		name = path.Base(ph.LogicalPath)
	}
	hasPreview := "false"
	if h.hasPreview != nil && h.hasPreview(ph.MIME) && ph.Width >= 0 {
		hasPreview = "true"
	}
	return davResponse{
		Href: href,
		Propstat: davPropstat{
			Prop: davProp{
				DisplayName:   name,
				ResourceType:  &resourceType{},
				ContentLength: &size,
				ContentType:   ph.MIME,
				LastModified:  ph.TakenAt.UTC().Format(http.TimeFormat),
				ETag:          fmt.Sprintf("%q", etagOfHash(ph.SHA256)),
				ID:            ocID(ph.ID),
				FileID:        strconv.FormatInt(ph.ID, 10),
				Permissions:   "RGDNVW",
				Size:          &size,
				HasPreview:    hasPreview,
				UploadTime:    ph.UploadedAt.Unix(),
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
	// Ngày gốc: X-OC-Mtime nếu có, nếu không suy từ tên file do app đặt
	// ("YY-MM-DD HH-MM-SS ...") để yyyymm/taken_at phản ánh NGÀY CHỤP, không
	// phải ngày upload.
	mtime := originalDate(parseOCMtime(r.Header.Get("X-OC-Mtime")), path.Base(rel))
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

	res, _, fileID, err := h.storeWithDedup(r.Context(), staged, path.Base(rel), mtime, rel)
	if err != nil {
		// storeWithDedup đã RemoveStaged ở nhánh lỗi finalize.
		h.logger.Error("PUT lưu lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	setUploadHeaders(w, fileID, res.SHA256)
	w.WriteHeader(http.StatusCreated) // 201
	h.logger.Info("đã lưu ảnh", "rel", res.RelPath, "size", res.Size, "sha256", res.SHA256[:12])
}

// storeWithDedup hoàn tất một file đã stage: tra hash toàn cục để quyết định
// đưa vào photos/ (bản gốc) hay recyclebin/ (bản trùng, is_trash=true, dup_of=gốc).
// Luôn trả về *Result khi không lỗi; dup=true nếu là bản trùng. Dù dedup hay
// không, một lỗi DB tra cứu KHÔNG bao giờ chặn backup — khi đó coi như không trùng.
// fileID là id trả cho app (OC-FileId): với bản trùng là id BẢN GỐC, vì đó là
// file app sẽ thấy khi duyệt server.
func (h *Handler) storeWithDedup(ctx context.Context, staged *storage.StageResult, baseName string, mtime time.Time, logicalPath string) (res *storage.Result, dup bool, fileID int64, err error) {
	h.dedupMu.Lock()
	defer h.dedupMu.Unlock()

	existing, err := h.pg.GetBySHA256(ctx, staged.SHA256)
	if err != nil {
		h.logger.Warn("tra cứu hash thất bại, coi như không trùng", "hash", staged.SHA256[:12], "err", err)
		existing = nil
	}

	if existing != nil {
		res, ferr := h.store.FinalizeStaged(staged.TempRel, "recyclebin", baseName, staged.SHA256, mtime)
		if ferr != nil {
			_ = h.store.RemoveStaged(staged.TempRel)
			return nil, false, 0, ferr
		}
		dupOf := existing.ID
		h.indexPhoto(ctx, res, mtime, true, &dupOf, logicalPath)
		h.logger.Warn("phát hiện trùng → đưa vào thùng rác", "hash", staged.SHA256[:12], "rel", res.RelPath, "gốc", existing.RelPath)
		return res, true, existing.ID, nil
	}

	res, ferr := h.store.FinalizeStaged(staged.TempRel, "photos", baseName, staged.SHA256, mtime)
	if ferr != nil {
		_ = h.store.RemoveStaged(staged.TempRel)
		return nil, false, 0, ferr
	}
	id := h.indexPhoto(ctx, res, mtime, false, nil, logicalPath)
	return res, false, id, nil
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
func (h *Handler) indexPhoto(ctx context.Context, res *storage.Result, mtime time.Time, isTrash bool, dupOf *int64, logicalPath string) int64 {
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
	id, err := h.pg.UpsertPhotoID(ctx, ph)
	if err != nil {
		h.logger.Warn("index Postgres thất bại", "rel", res.RelPath, "err", err)
	}
	return id
}

// setUploadHeaders ghi các header Nextcloud trả sau một upload thành công.
//
// QUAN TRỌNG: app iOS chỉ coi upload là thành công khi có OC-FileId. Thiếu nó,
// app bỏ qua bước "đã upload" — không ghi nhận auto-upload (nên lần sau
// upload lại từ đầu) và không bao giờ xoá ảnh khỏi máy ("xoá sau khi tải lên").
func setUploadHeaders(w http.ResponseWriter, fileID int64, sha string) {
	etag := fmt.Sprintf("%q", etagOfHash(sha))
	w.Header().Set("OC-ETag", etag)
	w.Header().Set("ETag", etag)
	if fileID > 0 {
		w.Header().Set("OC-FileId", ocID(fileID))
		w.Header().Set("X-NC-Permissions", "RGDNVW")
	}
}
