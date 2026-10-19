// Package dashboard phục vụ giao diện web xem ảnh đã backup: thumbnail + lọc ngày.
//
// Thumbnail chỉ sinh cho ảnh JPEG/PNG (thư viện chuẩn). HEIC/video hiển thị thẻ
// có icon + metadata, bấm vào mở file gốc. Thumbnail KHÔNG đụng file gốc.
package dashboard

import (
	"html/template"
	"image"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"net/http"
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
	cfg     *config.Config
	store   *storage.Storage
	pg      *store.Postgres
	logger  *slog.Logger
}

// New tạo dashboard.
func New(cfg *config.Config, st *storage.Storage, pg *store.Postgres, logger *slog.Logger) *Dashboard {
	return &Dashboard{cfg: cfg, store: st, pg: pg, logger: logger}
}

type viewItem struct {
	RelPath  string
	Name     string
	SizeKB   int64
	TakenAt  string
	IsImage  bool // JPEG/PNG → có thumbnail; còn lại hiển thị icon
	MIME     string
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
 .empty{padding:3rem;text-align:center;color:#888}
</style></head><body>
<header>
 <h1>Backup ảnh iPhone</h1>
 <span class="stat">{{.Count}} mục · user <code>{{.User}}</code></span>
 <form method="GET" action="/dashboard">
   <label>Ngày: <input type="date" name="date" value="{{.Date}}"></label>
   <button type="submit">Lọc</button>
   {{if .Date}}<a href="/dashboard">Bỏ lọc</a>{{end}}
 </form>
</header>
{{if .Items}}
<div class="grid">
 {{range .Items}}
 <a class="cell" href="/media?path={{.RelPath}}" target="_blank">
   {{if .IsImage}}<img loading="lazy" src="/thumb?path={{.RelPath}}" alt="{{.Name}}">
   {{else}}<div class="icon">{{if eq .MIME "video/quicktime"}}🎞️{{else if eq .MIME "video/mp4"}}🎞️{{else}}🖼️{{end}}</div>{{end}}
   <div class="meta"><div class="nm">{{.Name}}</div><div class="sub">{{.TakenAt}} · {{.SizeKB}} KB</div></div>
 </a>
 {{end}}
</div>
{{else}}
<div class="empty">Chưa có ảnh nào{{if .Date}} trong ngày này{{end}}. Bật Auto Upload trên app Nextcloud để bắt đầu backup.</div>
{{end}}
</body></html>`))

// Index phục vụ GET /dashboard (?date=YYYY-MM-DD để lọc).
func (d *Dashboard) Index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dateStr := r.URL.Query().Get("date")

	var photos []store.Photo
	var err error
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

	count, _ := d.pg.Count(ctx)

	items := make([]viewItem, 0, len(photos))
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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		User  string
		Count int64
		Date  string
		Items []viewItem
	}{d.cfg.User, count, dateStr, items}
	if err := page.Execute(w, data); err != nil {
		d.logger.Error("render dashboard lỗi", "err", err)
	}
}

// Thumb phục vụ GET /thumb?path=... — sinh thumbnail JPEG 300px cho ảnh JPEG/PNG.
func (d *Dashboard) Thumb(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "thiếu path", http.StatusBadRequest)
		return
	}
	f, err := d.store.Open(rel)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		http.Error(w, "không decode được ảnh", http.StatusUnsupportedMediaType)
		return
	}

	const maxW = 300
	b := src.Bounds()
	w2, h2 := b.Dx(), b.Dy()
	if w2 > maxW {
		h2 = h2 * maxW / w2
		w2 = maxW
	}
	dst := image.NewRGBA(image.Rect(0, 0, w2, h2))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_ = jpeg.Encode(w, dst, &jpeg.Options{Quality: 80})
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
