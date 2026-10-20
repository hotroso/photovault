// Package dashboard phục vụ giao diện web xem ảnh đã backup: thumbnail + lọc ngày.
//
// Thumbnail chỉ sinh cho ảnh JPEG/PNG (thư viện chuẩn). HEIC/video hiển thị thẻ
// có icon + metadata, bấm vào mở file gốc. Thumbnail KHÔNG đụng file gốc.
package dashboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/storage"
	"github.com/local/photovault/internal/store"
)

// Dashboard gom phụ thuộc cho các trang xem ảnh.
type Dashboard struct {
	cfg    *config.Config
	store  *storage.Storage
	pg     *store.Postgres
	logger *slog.Logger

	// thumbSem giới hạn số thumbnail được decode đồng thời. Decode một ảnh
	// 48MP tốn ~75–200MB RAM; trình duyệt (HTTP/2 qua Caddy) có thể bắn hàng
	// chục request /thumb cùng lúc → không giới hạn sẽ OOM.
	thumbSem chan struct{}
}

const (
	thumbMaxW        = 300        // chiều rộng thumbnail (px)
	thumbMaxPixels   = 64_000_000 // từ chối ảnh > 64MP (chống decompression bomb; iPhone 48MP vẫn qua)
	thumbConcurrency = 2          // số decode chạy song song tối đa
	thumbCacheDir    = ".thumbs"  // cache trong DataDir; reindex/verify đã bỏ qua thư mục này
)

var errThumbTooLarge = errors.New("ảnh quá lớn để tạo thumbnail")

// New tạo dashboard.
func New(cfg *config.Config, st *storage.Storage, pg *store.Postgres, logger *slog.Logger) *Dashboard {
	return &Dashboard{
		cfg:      cfg,
		store:    st,
		pg:       pg,
		logger:   logger,
		thumbSem: make(chan struct{}, thumbConcurrency),
	}
}

type viewItem struct {
	RelPath string
	Name    string
	SizeKB  int64
	TakenAt string
	IsImage bool // JPEG/PNG → có thumbnail; còn lại hiển thị icon
	MIME    string
	IsTrash bool   // true khi hiển thị trong trang thùng rác
	DupInfo string // tên bản gốc mà file này trùng (chỉ dùng ở trang rác)
}

var page = template.Must(template.New("dash").Parse(`<!doctype html>
<html lang="vi"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Backup ảnh iPhone</title>
<style>
 body{font-family:system-ui,sans-serif;margin:0;background:#fafafa;color:#1a1a1a}
 header{padding:1rem 1.5rem;background:#fff;border-bottom:1px solid #eee;display:flex;gap:1rem;align-items:center;flex-wrap:wrap}
 h1{font-size:1.1rem;margin:0}
 .stat{color:#666;font-size:.9rem}
 form{margin-left:auto;display:flex;gap:.5rem;align-items:center}
 input[type=date]{padding:.35rem;border:1px solid #ccc;border-radius:6px}
 .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:12px;padding:1.5rem}
 .cell{background:#fff;border:1px solid #eee;border-radius:10px;overflow:hidden;text-decoration:none;color:inherit}
 .cell img{width:100%;height:150px;object-fit:cover;display:block;background:#f0f0f0}
 .icon{width:100%;height:150px;display:grid;place-items:center;font-size:2.5rem;background:#eef2f5}
 .meta{padding:.5rem;font-size:.75rem}
 .meta .nm{font-weight:600;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
 .meta .sub{color:#888}
 .meta .dup{color:#b45309;font-style:italic;margin-top:.15rem}
 .empty{padding:3rem;text-align:center;color:#888}
 nav{display:flex;gap:.75rem}
 nav a{text-decoration:none;color:#555;padding:.35rem .6rem;border-radius:6px}
 nav a.active{background:#1a1a1a;color:#fff}
 .actions{display:flex;gap:.4rem;padding:0 .5rem .5rem}
 .actions form{margin:0}
 .actions button{flex:1;padding:.3rem;font-size:.7rem;border:1px solid #ccc;border-radius:6px;background:#f7f7f7;cursor:pointer}
 .actions button.danger{color:#b00020;border-color:#f0c0c8}
</style></head><body>
<header>
 <h1>Backup ảnh iPhone</h1>
 <nav>
   <a href="/dashboard" class="{{if not .Trash}}active{{end}}">Ảnh</a>
   <a href="/dashboard?view=trash" class="{{if .Trash}}active{{end}}">Thùng rác ({{.TrashCount}})</a>
 </nav>
 <span class="stat">{{.Count}} mục · user <code>{{.User}}</code></span>
 {{if not .Trash}}
 <form method="GET" action="/dashboard">
   <label>Ngày: <input type="date" name="date" value="{{.Date}}"></label>
   <button type="submit">Lọc</button>
   {{if .Date}}<a href="/dashboard">Bỏ lọc</a>{{end}}
 </form>
 {{end}}
</header>
{{if .Items}}
<div class="grid">
 {{range .Items}}
 {{if .IsTrash}}
 <div class="cell">
   <a href="/media?path={{.RelPath}}" target="_blank">
     {{if .IsImage}}<img loading="lazy" src="/thumb?path={{.RelPath}}" alt="{{.Name}}">
     {{else}}<div class="icon">{{if eq .MIME "video/quicktime"}}🎞️{{else if eq .MIME "video/mp4"}}🎞️{{else}}🖼️{{end}}</div>{{end}}
   </a>
   <div class="meta"><div class="nm">{{.Name}}</div><div class="sub">{{.TakenAt}} · {{.SizeKB}} KB</div><div class="dup">trùng với {{if .DupInfo}}{{.DupInfo}}{{else}}(không rõ bản gốc){{end}}</div></div>
   <div class="actions">
     <form method="POST" action="/trash/restore"><input type="hidden" name="rel" value="{{.RelPath}}"><button type="submit">Khôi phục</button></form>
     <form method="POST" action="/trash/delete"><input type="hidden" name="rel" value="{{.RelPath}}"><button type="submit" class="danger">Xoá vĩnh viễn</button></form>
   </div>
 </div>
 {{else}}
 <a class="cell" href="/media?path={{.RelPath}}" target="_blank">
   {{if .IsImage}}<img loading="lazy" src="/thumb?path={{.RelPath}}" alt="{{.Name}}">
   {{else}}<div class="icon">{{if eq .MIME "video/quicktime"}}🎞️{{else if eq .MIME "video/mp4"}}🎞️{{else}}🖼️{{end}}</div>{{end}}
   <div class="meta"><div class="nm">{{.Name}}</div><div class="sub">{{.TakenAt}} · {{.SizeKB}} KB</div></div>
 </a>
 {{end}}
 {{end}}
</div>
{{else}}
<div class="empty">{{if .Trash}}Thùng rác trống.{{else}}Chưa có ảnh nào{{if .Date}} trong ngày này{{end}}. Bật Auto Upload trên app Nextcloud để bắt đầu backup.{{end}}</div>
{{end}}
</body></html>`))

// Index phục vụ GET /dashboard (?date=YYYY-MM-DD để lọc, ?view=trash xem thùng rác).
func (d *Dashboard) Index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dateStr := r.URL.Query().Get("date")
	trashView := r.URL.Query().Get("view") == "trash"

	// Danh sách thùng rác dùng để vừa hiển thị tab đếm, vừa (ở chế độ trash) làm lưới.
	trash, err := d.pg.ListTrash(ctx, 2000)
	if err != nil {
		d.logger.Error("truy vấn thùng rác lỗi", "err", err)
	}
	trashCount := len(trash)

	items := make([]viewItem, 0)
	if trashView {
		for _, p := range trash {
			dupInfo := ""
			if p.DupOf != nil {
				if orig, gerr := d.pg.GetByID(ctx, *p.DupOf); gerr != nil {
					d.logger.Error("tra bản gốc của file trùng lỗi", "err", gerr, "dup_of", *p.DupOf)
				} else if orig != nil {
					dupInfo = orig.OriginalName
					if dupInfo == "" {
						dupInfo = orig.RelPath
					}
				}
			}
			items = append(items, viewItem{
				RelPath: p.RelPath,
				Name:    p.OriginalName,
				SizeKB:  p.Size / 1024,
				TakenAt: p.TakenAt.Local().Format("2006-01-02 15:04"),
				IsImage: isThumbnailable(p.MIME),
				MIME:    p.MIME,
				IsTrash: true,
				DupInfo: dupInfo,
			})
		}
	} else {
		var photos []store.Photo
		if dateStr != "" {
			var day time.Time
			day, err = time.ParseInLocation("2006-01-02", dateStr, time.Local)
			if err == nil {
				photos, err = d.pg.ListByDate(ctx, day, day.AddDate(0, 0, 1), 2000)
			}
		} else {
			photos, err = d.pg.ListAll(ctx, 2000)
		}
		if err != nil {
			d.logger.Error("truy vấn dashboard lỗi", "err", err)
		}
		for _, p := range photos {
			items = append(items, viewItem{
				RelPath: p.RelPath,
				Name:    p.OriginalName,
				SizeKB:  p.Size / 1024,
				TakenAt: p.TakenAt.Local().Format("2006-01-02 15:04"),
				IsImage: isThumbnailable(p.MIME),
				MIME:    p.MIME,
			})
		}
	}

	count, _ := d.pg.Count(ctx)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		User       string
		Count      int64
		Date       string
		Trash      bool
		TrashCount int
		Items      []viewItem
	}{d.cfg.User, count, dateStr, trashView, trashCount, items}
	if err := page.Execute(w, data); err != nil {
		d.logger.Error("render dashboard lỗi", "err", err)
	}
}

// TrashDelete phục vụ POST /trash/delete — xoá vĩnh viễn một file trong thùng rác
// khỏi đĩa VÀ khỏi chỉ mục. Chỉ chấp nhận rel nằm dưới recyclebin/.
func (d *Dashboard) TrashDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form không hợp lệ", http.StatusBadRequest)
		return
	}
	rel := r.FormValue("rel")
	if !isRecyclebinRel(rel) {
		http.Error(w, "chỉ xoá được file trong thùng rác", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if err := d.store.Remove(rel); err != nil {
		d.logger.Error("xoá file thùng rác khỏi đĩa lỗi", "err", err, "rel", rel)
		http.Error(w, "xoá file lỗi", http.StatusInternalServerError)
		return
	}
	if err := d.pg.DeletePhotoByRelPath(ctx, rel); err != nil {
		d.logger.Error("xoá bản ghi thùng rác khỏi DB lỗi", "err", err, "rel", rel)
	}
	d.logger.Info("đã xoá vĩnh viễn file trong thùng rác", "rel", rel)
	http.Redirect(w, r, "/dashboard?view=trash", http.StatusSeeOther)
}

// TrashRestore phục vụ POST /trash/restore — khôi phục một file từ recyclebin/
// về photos/ (collision-safe), cập nhật is_trash=false, dup_of=NULL. File di
// chuyển trên đĩa là source of truth; DB chỉ chạy theo. Chỉ nhận rel dưới recyclebin/.
func (d *Dashboard) TrashRestore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form không hợp lệ", http.StatusBadRequest)
		return
	}
	rel := r.FormValue("rel")
	if !isRecyclebinRel(rel) {
		http.Error(w, "chỉ khôi phục được file trong thùng rác", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// Giữ ngày gốc: lấy mtime từ file trên đĩa (MoveCollisionSafe cũng giữ yyyymm từ rel).
	var mtime time.Time
	if fi, _, serr := d.store.Stat(rel); serr == nil {
		mtime = fi.ModTime()
	}

	res, err := d.store.MoveCollisionSafe(rel, "photos", mtime)
	if err != nil {
		d.logger.Error("khôi phục file khỏi thùng rác lỗi", "err", err, "rel", rel)
		http.Error(w, "khôi phục lỗi", http.StatusInternalServerError)
		return
	}
	if err := d.pg.SetTrashState(ctx, rel, res.RelPath, false, nil); err != nil {
		d.logger.Error("cập nhật trạng thái khôi phục trong DB lỗi", "err", err, "rel", rel, "new", res.RelPath)
	}
	d.logger.Info("đã khôi phục file khỏi thùng rác", "từ", rel, "đến", res.RelPath)
	http.Redirect(w, r, "/dashboard?view=trash", http.StatusSeeOther)
}

// Thumb phục vụ GET /thumb?path=... — thumbnail JPEG 300px cho ảnh JPEG/PNG.
//
// Chống quá tải:
//   - Cache trên đĩa (.thumbs/): mỗi ảnh chỉ decode MỘT lần, các lần xem sau
//     đọc file nhỏ có sẵn.
//   - Semaphore: tối đa thumbConcurrency decode cùng lúc; request khác xếp hàng
//     và tự huỷ nếu trình duyệt bỏ request (cuộn qua).
//   - DecodeConfig trước: từ chối ảnh quá thumbMaxPixels trước khi cấp phát RAM.
func (d *Dashboard) Thumb(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "thiếu path", http.StatusBadRequest)
		return
	}
	fi, _, err := d.store.Stat(rel)
	if err != nil || fi.IsDir() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	cacheRel := thumbCacheRel(rel, fi)
	if d.serveCachedThumb(w, r, cacheRel) {
		return
	}

	// Chờ lượt decode; huỷ ngay nếu client đã bỏ request.
	select {
	case d.thumbSem <- struct{}{}:
		defer func() { <-d.thumbSem }()
	case <-r.Context().Done():
		return
	}

	// Trong lúc chờ, một request khác có thể đã sinh xong thumbnail này.
	if d.serveCachedThumb(w, r, cacheRel) {
		return
	}

	data, err := d.renderThumb(rel)
	if err != nil {
		if errors.Is(err, errThumbTooLarge) {
			d.logger.Warn("bỏ qua thumbnail ảnh quá lớn", "rel", rel)
			http.Error(w, "ảnh quá lớn", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "không decode được ảnh", http.StatusUnsupportedMediaType)
		return
	}

	// Ghi cache atomic; lỗi cache không chặn việc trả thumbnail.
	if _, werr := d.store.WriteAtomic(cacheRel, bytes.NewReader(data), int64(len(data)), time.Time{}); werr != nil {
		d.logger.Warn("ghi cache thumbnail lỗi", "rel", rel, "err", werr)
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// renderThumb decode ảnh gốc và thu nhỏ về thumbMaxW, trả JPEG đã encode.
func (d *Dashboard) renderThumb(rel string) ([]byte, error) {
	f, err := d.store.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Đọc header kích thước trước — rẻ, không decode pixel.
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > thumbMaxPixels {
		return nil, errThumbTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}

	b := src.Bounds()
	w2, h2 := b.Dx(), b.Dy()
	if w2 > thumbMaxW {
		h2 = h2 * thumbMaxW / w2
		w2 = thumbMaxW
	}
	if w2 < 1 {
		w2 = 1
	}
	if h2 < 1 {
		h2 = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w2, h2))
	// ApproxBiLinear nhanh hơn CatmullRom nhiều lần; ở kích thước 300px khác
	// biệt chất lượng gần như không thấy.
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// serveCachedThumb trả thumbnail đã cache nếu có. Trả false nếu chưa có cache.
func (d *Dashboard) serveCachedThumb(w http.ResponseWriter, r *http.Request, cacheRel string) bool {
	cf, err := d.store.Open(cacheRel)
	if err != nil {
		return false
	}
	defer cf.Close()
	st, err := cf.Stat()
	if err != nil || st.Size() == 0 {
		return false
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", st.ModTime(), cf)
	return true
}

// thumbCacheRel tính đường dẫn cache cho thumbnail của rel. Khoá gồm rel + size
// + mtime nên file đổi nội dung/di chuyển sẽ sinh thumbnail mới. Chia thư mục con
// theo 2 ký tự đầu để tránh một thư mục chứa hàng chục nghìn file.
func thumbCacheRel(rel string, fi os.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", rel, fi.Size(), fi.ModTime().UnixNano())))
	key := hex.EncodeToString(sum[:16])
	return thumbCacheDir + "/" + key[:2] + "/" + key + ".jpg"
}

// Media phục vụ GET /media?path=... — trả file gốc (xem full-res / tải video).
func (d *Dashboard) Media(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "thiếu path", http.StatusBadRequest)
		return
	}
	fi, _, err := d.store.Stat(rel)
	if err != nil || fi.IsDir() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	f, err := d.store.Open(rel)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

func isThumbnailable(mime string) bool {
	return strings.EqualFold(mime, "image/jpeg") || strings.EqualFold(mime, "image/png")
}

// isRecyclebinRel xác nhận rel sau khi làm sạch (giống storage.safeJoin) vẫn có
// segment cấp cao nhất là "recyclebin". Chống bypass kiểu
// "recyclebin/../photos/202303/x.jpg": HasPrefix lọt nhưng filepath.Clean đưa nó
// về "photos/..." — một bản gốc còn sống, KHÔNG nằm trong thùng rác.
func isRecyclebinRel(rel string) bool {
	clean := filepath.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))
	clean = strings.TrimPrefix(clean, "/")
	clean = filepath.ToSlash(clean)
	seg, _, _ := strings.Cut(clean, "/")
	return seg == "recyclebin"
}
