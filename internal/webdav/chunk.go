package webdav

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ServeUploads điều phối chunked upload v2 trên /remote.php/dav/uploads/<user>/...
//
// Luồng (theo tài liệu Nextcloud):
//   MKCOL  /uploads/<user>/<uuid>            (+ Destination)  → tạo thư mục chunk
//   PUT    /uploads/<user>/<uuid>/00001      (+ Destination)  → ghi từng chunk
//   MOVE   /uploads/<user>/<uuid>/.file      (+ Destination)  → ghép → file đích
//   DELETE /uploads/<user>/<uuid>                              → huỷ
func (h *Handler) ServeUploads(w http.ResponseWriter, r *http.Request) {
	rel := h.relFromUploads(r.URL.Path) // ví dụ "<user>/<uuid>" hoặc "<user>/<uuid>/00001"
	switch r.Method {
	case "MKCOL":
		h.uploadMkcol(w, rel)
	case "PUT":
		h.uploadPutChunk(w, r, rel)
	case "MOVE":
		h.uploadAssemble(w, r, rel)
	case "DELETE":
		h.uploadAbort(w, rel)
	case "PROPFIND":
		// Một số client PROPFIND thư mục upload; trả 207 rỗng là đủ.
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"></d:multistatus>`))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// uploadsRoot: nơi chứa thư mục chunk tạm, nằm trong DataDir dưới .uploads/
func (h *Handler) relFromUploads(urlPath string) string {
	p := strings.TrimPrefix(urlPath, "/remote.php/dav/uploads/")
	return strings.Trim(p, "/")
}

// chunkDirRel ánh xạ "<user>/<uuid>[/chunk]" → đường lưu tạm ".uploads/<user>/<uuid>[/chunk]"
func chunkDirRel(rel string) string {
	return filepath.ToSlash(filepath.Join(".uploads", rel))
}

func (h *Handler) uploadMkcol(w http.ResponseWriter, rel string) {
	if err := h.store.Mkdir(chunkDirRel(rel)); err != nil {
		h.logger.Error("upload MKCOL lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) uploadPutChunk(w http.ResponseWriter, r *http.Request, rel string) {
	// rel = "<user>/<uuid>/<chunkName>"
	expected := int64(-1)
	if r.ContentLength > 0 {
		expected = r.ContentLength
	}
	res, err := h.store.WriteAtomic(chunkDirRel(rel), r.Body, expected, time.Time{})
	if err != nil {
		h.logger.Error("upload PUT chunk lỗi", "rel", rel, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("OC-ETag", fmt.Sprintf("%q", res.SHA256[:16]))
	w.WriteHeader(http.StatusCreated)
}

// uploadAssemble xử lý MOVE .../.file → ghép mọi chunk theo thứ tự tên → file đích.
func (h *Handler) uploadAssemble(w http.ResponseWriter, r *http.Request, rel string) {
	dest := destRelFromHeader(r.Header.Get("Destination"))
	if dest == "" {
		http.Error(w, "thiếu Destination", http.StatusBadRequest)
		return
	}
	// Ngày gốc: X-OC-Mtime nếu có, nếu không suy từ tên file đích (video lớn
	// cũng theo mẫu "YY-MM-DD HH-MM-SS ..."). Xem originalDate trong helpers.go.
	mtime := originalDate(parseOCMtime(r.Header.Get("X-OC-Mtime")), filepath.Base(dest))

	// rel = "<user>/<uuid>/.file" → thư mục chunk là "<user>/<uuid>"
	uuidRel := strings.TrimSuffix(rel, "/.file")
	uuidRel = strings.TrimSuffix(uuidRel, "/")

	chunkDirAbs := filepath.Join(h.store.Root(), filepath.FromSlash(chunkDirRel(uuidRel)))
	entries, err := os.ReadDir(chunkDirAbs)
	if err != nil {
		h.logger.Error("đọc thư mục chunk lỗi", "dir", chunkDirAbs, "err", err)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	// Lọc file chunk (bỏ .part), sắp theo tên (00001, 00002, ...).
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") || e.Name() == ".file" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		http.Error(w, "không có chunk", http.StatusBadRequest)
		return
	}

	// Tạo reader nối tiếp các chunk.
	readers := make([]io.Reader, 0, len(names))
	files := make([]*os.File, 0, len(names))
	for _, n := range names {
		f, err := os.Open(filepath.Join(chunkDirAbs, n))
		if err != nil {
			closeAll(files)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	multi := io.MultiReader(readers...)

	// Kiểm tra tổng kích thước nếu client gửi OC-Total-Length.
	expected := int64(-1)
	if tl := r.Header.Get("OC-Total-Length"); tl != "" {
		if v, perr := parseInt64(tl); perr == nil {
			expected = v
		}
	}

	// Stage trước (hash một lần) rồi quyết định photos/ hay recyclebin/ theo dedup.
	staged, err := h.store.StageTemp(multi, expected)
	closeAll(files)
	if err != nil {
		h.logger.Error("ghép & stage chunk lỗi", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Dọn thư mục chunk tạm.
	_ = h.store.Remove(chunkDirRel(uuidRel))

	res, _, err := h.storeWithDedup(r.Context(), staged, filepath.Base(dest), mtime, dest)
	if err != nil {
		h.logger.Error("lưu file ghép lỗi", "dest", dest, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("OC-ETag", fmt.Sprintf("%q", res.SHA256[:16]))
	w.WriteHeader(http.StatusCreated) // 201
	h.logger.Info("đã ghép & lưu video/ảnh lớn", "dest", res.RelPath, "size", res.Size, "chunks", len(names), "sha256", res.SHA256[:12])
}

func (h *Handler) uploadAbort(w http.ResponseWriter, rel string) {
	// rel có thể là "<user>/<uuid>" hoặc có chunk phía sau; xoá cả thư mục uuid.
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 {
		rel = parts[0] + "/" + parts[1]
	}
	_ = h.store.Remove(chunkDirRel(rel))
	w.WriteHeader(http.StatusNoContent)
}

func closeAll(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

func parseInt64(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	return v, err
}
