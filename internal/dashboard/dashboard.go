// Package dashboard phục vụ giao diện web xem ảnh đã backup: thumbnail + lọc ngày.
//
// Thumbnail chỉ sinh cho ảnh JPEG/PNG (thư viện chuẩn). HEIC/video hiển thị thẻ
// có icon + metadata, bấm vào mở file gốc. Thumbnail KHÔNG đụng file gốc.
package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/jpeg"
	_ "image/png"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	// Thumbnail theo CHIỀU CAO vì gallery xếp theo hàng: 360px đủ nét cho hàng
	// ~180–230px trên màn hình HiDPI. Ảnh panorama giới hạn chiều rộng.
	thumbHeight      = 360
	thumbMaxWidth    = 1080
	thumbMaxPixels   = 64_000_000 // từ chối ảnh > 64MP (chống decompression bomb; iPhone 48MP vẫn qua)
	thumbConcurrency = 2          // số decode chạy song song tối đa
	thumbCacheDir    = ".thumbs"  // cache trong DataDir; reindex/verify đã bỏ qua thư mục này
	thumbCacheVer    = "v2"       // đổi khi thay cách sinh thumbnail để bỏ cache cũ

	pageDefault = 150 // số ảnh mỗi trang của /api/photos
	pageMax     = 500
)

//go:embed static
var staticFiles embed.FS

// StaticHandler phục vụ /static/ (JS/CSS gallery nhúng trong binary).
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err) // chỉ xảy ra nếu go:embed sai đường dẫn — lỗi lúc build
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// File nhỏ; no-cache để deploy bản mới là trình duyệt nhận ngay.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

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
 header{position:sticky;top:0;z-index:10;padding:1rem 1.5rem;background:rgba(255,255,255,.95);backdrop-filter:blur(6px);border-bottom:1px solid #eee;display:flex;gap:1rem;align-items:center;flex-wrap:wrap}
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
</style>
<link rel="stylesheet" href="/static/gallery.css">
</head><body>
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
{{if not .Trash}}
<main class="jg-wrap">
 <div id="gallery" data-date="{{.Date}}"></div>
 <p id="gallery-status" role="status" aria-live="polite"></p>
</main>
<div id="lb" class="lb" role="dialog" aria-modal="true" aria-label="Xem ảnh" hidden>
 <div class="lb-stage"></div>
 <button type="button" class="lb-btn lb-close" aria-label="Đóng">✕</button>
 <button type="button" class="lb-btn lb-prev" aria-label="Ảnh trước">‹</button>
 <button type="button" class="lb-btn lb-next" aria-label="Ảnh sau">›</button>
 <div class="lb-bar"><span class="lb-cap"></span><a class="lb-open" target="_blank" rel="noopener">Mở file gốc</a></div>
</div>
<script src="/static/gallery.js" defer></script>
{{else if .Items}}
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
<div class="empty">Thùng rác trống.</div>
{{end}}
</body></html>`))

// Index phục vụ GET /dashboard (?date=YYYY-MM-DD để lọc, ?view=trash xem thùng rác).
//
// Trang ảnh chính chỉ trả khung HTML; ảnh được gallery.js tải dần qua /api/photos
// (cuộn vô hạn). Trang thùng rác vẫn render phía server vì số lượng nhỏ.
func (d *Dashboard) Index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dateStr := r.URL.Query().Get("date")
	if dateStr != "" {
		if _, err := time.ParseInLocation("2006-01-02", dateStr, time.Local); err != nil {
			dateStr = "" // ngày không hợp lệ → bỏ lọc
		}
	}
	trashView := r.URL.Query().Get("view") == "trash"

	trashCount, err := d.pg.CountByTrash(ctx, true)
	if err != nil {
		d.logger.Error("đếm thùng rác lỗi", "err", err)
	}
	count, err := d.pg.CountByTrash(ctx, false)
	if err != nil {
		d.logger.Error("đếm ảnh lỗi", "err", err)
	}

	items := make([]viewItem, 0)
	if trashView {
		trash, err := d.pg.ListTrash(ctx, 2000)
		if err != nil {
			d.logger.Error("truy vấn thùng rác lỗi", "err", err)
		}
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
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		User       string
		Count      int64
		Date       string
		Trash      bool
		TrashCount int64
		Items      []viewItem
	}{d.cfg.User, count, dateStr, trashView, trashCount, items}
	if err := page.Execute(w, data); err != nil {
		d.logger.Error("render dashboard lỗi", "err", err)
	}
}

// apiPhoto là một mục trả cho gallery.js.
type apiPhoto struct {
	Path   string `json:"path"`   // rel_path vật lý, dùng cho /thumb và /media
	Name   string `json:"name"`   // tên gốc
	Taken  string `json:"taken"`  // "2006-01-02T15:04" giờ địa phương (7 ký tự đầu = khoá tháng)
	Label  string `json:"label"`  // ngày giờ hiển thị
	SizeKB int64  `json:"sizeKB"` // dung lượng
	Kind   string `json:"kind"`   // "image" (có thumbnail) | "video" | "other"
	W      int    `json:"w"`      // kích thước hiển thị đã xoay EXIF; 0 = không rõ
	H      int    `json:"h"`
}

// APIPhotos phục vụ GET /api/photos?cursor=&limit=&date= — một trang ảnh cho
// gallery cuộn vô hạn. Phân trang keyset theo (taken_at, id): trả "next" để lấy
// trang sau, rỗng khi hết.
func (d *Dashboard) APIPhotos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	limit := pageDefault
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = min(v, pageMax)
	}

	var after *store.PageCursor
	if c := q.Get("cursor"); c != "" {
		pc, ok := parseCursor(c)
		if !ok {
			http.Error(w, "cursor không hợp lệ", http.StatusBadRequest)
			return
		}
		after = &pc
	}

	var from, to *time.Time
	if ds := q.Get("date"); ds != "" {
		day, err := time.ParseInLocation("2006-01-02", ds, time.Local)
		if err != nil {
			http.Error(w, "date không hợp lệ", http.StatusBadRequest)
			return
		}
		next := day.AddDate(0, 0, 1)
		from, to = &day, &next
	}

	// Lấy dư 1 bản ghi để biết còn trang sau hay không.
	photos, err := d.pg.ListPage(ctx, after, from, to, limit+1)
	if err != nil {
		d.logger.Error("truy vấn trang ảnh lỗi", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hasMore := len(photos) > limit
	if hasMore {
		photos = photos[:limit]
	}
	d.fillDimensions(ctx, photos)

	resp := struct {
		Items []apiPhoto `json:"items"`
		Next  string     `json:"next"`
	}{Items: make([]apiPhoto, 0, len(photos))}
	for _, p := range photos {
		t := p.TakenAt.Local()
		item := apiPhoto{
			Path:   p.RelPath,
			Name:   p.OriginalName,
			Taken:  t.Format("2006-01-02T15:04"),
			Label:  t.Format("02/01/2006 15:04"),
			SizeKB: p.Size / 1024,
			Kind:   mediaKind(p.MIME),
		}
		if p.Width > 0 && p.Height > 0 {
			item.W, item.H = p.Width, p.Height
		}
		resp.Items = append(resp.Items, item)
	}
	if hasMore {
		last := photos[len(photos)-1]
		resp.Next = fmt.Sprintf("%d_%d", last.TakenAt.UnixNano(), last.ID)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		d.logger.Warn("ghi JSON trang ảnh lỗi", "err", err)
	}
}

// parseCursor đọc cursor dạng "<taken_at unix nano>_<id>".
func parseCursor(s string) (store.PageCursor, bool) {
	ns, id, ok := strings.Cut(s, "_")
	if !ok {
		return store.PageCursor{}, false
	}
	n, err1 := strconv.ParseInt(ns, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil {
		return store.PageCursor{}, false
	}
	return store.PageCursor{TakenAt: time.Unix(0, n), ID: i}, true
}

// fillDimensions đo kích thước cho các ảnh chưa có (lần đầu xem) rồi lưu vào DB,
// để những lần sau khỏi đọc file. Chỉ đọc header + EXIF, không decode pixel;
// tối đa 4 file song song.
func (d *Dashboard) fillDimensions(ctx context.Context, photos []store.Photo) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range photos {
		p := &photos[i]
		if p.Width != 0 || !isThumbnailable(p.MIME) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p.Width, p.Height = d.measure(p.RelPath)
			if err := d.pg.SetDimensions(ctx, p.ID, p.Width, p.Height); err != nil {
				d.logger.Warn("lưu kích thước ảnh lỗi", "rel", p.RelPath, "err", err)
			}
		}()
	}
	wg.Wait()
}

// measure trả kích thước hiển thị (đã xoay theo EXIF) của ảnh; -1,-1 nếu không đọc được.
func (d *Dashboard) measure(rel string) (int, int) {
	f, err := d.store.Open(rel)
	if err != nil {
		return -1, -1
	}
	defer f.Close()
	cfg, _, orientation, err := imageMeta(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return -1, -1
	}
	if swapsAxes(orientation) {
		return cfg.Height, cfg.Width
	}
	return cfg.Width, cfg.Height
}

// mediaKind phân loại để gallery biết hiện thumbnail hay ô thay thế.
func mediaKind(mime string) string {
	switch {
	case isThumbnailable(mime):
		return "image"
	case strings.HasPrefix(strings.ToLower(mime), "video/"):
		return "video"
	default:
		return "other"
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

// Thumb phục vụ GET /thumb?path=... — thumbnail JPEG (cao 360px) cho ảnh JPEG/PNG.
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

// renderThumb decode ảnh gốc, thu nhỏ về cao thumbHeight, xoay theo EXIF
// Orientation, trả JPEG đã encode.
func (d *Dashboard) renderThumb(rel string) ([]byte, error) {
	f, err := d.store.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Đọc header kích thước + EXIF trước — rẻ, không decode pixel.
	cfg, _, orientation, err := imageMeta(f)
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > thumbMaxPixels {
		return nil, errThumbTooLarge
	}

	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}

	// Tính kích thước theo hướng HIỂN THỊ, rồi scale ở hướng lưu trữ và xoay
	// sau cùng (xoay trên ảnh nhỏ thì rẻ).
	swap := swapsAxes(orientation)
	dispW, dispH := cfg.Width, cfg.Height
	if swap {
		dispW, dispH = dispH, dispW
	}
	tw, th := thumbSize(dispW, dispH)
	pw, ph := tw, th
	if swap {
		pw, ph = th, tw
	}

	scaled := image.NewRGBA(image.Rect(0, 0, pw, ph))
	// ApproxBiLinear nhanh hơn CatmullRom nhiều lần; ở cỡ thumbnail khác biệt
	// chất lượng gần như không thấy. draw.Src để PNG trong suốt không thành đen.
	draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), src, src.Bounds(), draw.Src, nil)
	out := applyOrientation(scaled, orientation)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// thumbSize tính kích thước thumbnail: cao thumbHeight, rộng tối đa
// thumbMaxWidth, không phóng to ảnh nhỏ.
func thumbSize(w, h int) (int, int) {
	s := float64(thumbHeight) / float64(h)
	if float64(w)*s > thumbMaxWidth {
		s = thumbMaxWidth / float64(w)
	}
	if s > 1 {
		s = 1
	}
	return max(1, int(math.Round(float64(w)*s))), max(1, int(math.Round(float64(h)*s)))
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
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d", thumbCacheVer, rel, fi.Size(), fi.ModTime().UnixNano())))
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
