package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/photovault/internal/store"
)

// Sinh thumbnail:
//   - Ảnh: vipsthumbnail (libvips) nếu có. Với JPEG nó decode ở độ phân giải đã
//     thu nhỏ (shrink-on-load) nên nhanh và ít RAM hơn nhiều so với decode cả ảnh
//     bằng Go, tự xoay theo EXIF, đọc được cả HEIC. Không có vips → decoder Go.
//   - Video: ffmpeg cắt một khung hình làm ảnh bìa.
//
// Kết quả ghi thẳng vào cache .thumbs/ (file tạm rồi rename), nên request xem
// và bộ sinh nền dùng chung một chỗ.

const (
	thumbTimeout     = 60 * time.Second  // request xem: quá lâu thì bỏ
	warmItemTimeout  = 120 * time.Second // bộ sinh nền: mỗi file
	warmRescanPeriod = 2 * time.Minute   // quét lại để bắt ảnh mới upload
	warmPageSize     = 500
)

var errNoThumbnailer = errors.New("không có công cụ tạo thumbnail cho định dạng này")

// thumbTools là các chương trình ngoài tìm thấy lúc khởi động (rỗng = không có).
type thumbTools struct {
	vips   string
	ffmpeg string
	nice   string
}

func detectThumbTools() thumbTools {
	look := func(name string) string {
		p, err := exec.LookPath(name)
		if err != nil {
			return ""
		}
		return p
	}
	return thumbTools{vips: look("vipsthumbnail"), ffmpeg: look("ffmpeg"), nice: look("nice")}
}

// canThumb cho biết có tạo được thumbnail cho định dạng này không.
func (d *Dashboard) canThumb(mime string) bool {
	m := strings.ToLower(mime)
	switch {
	case m == "image/jpeg" || m == "image/png":
		return true // decoder Go luôn có
	case m == "image/heic" || m == "image/heif" || m == "image/webp":
		return d.tools.vips != ""
	case strings.HasPrefix(m, "video/"):
		return d.tools.ffmpeg != ""
	}
	return false
}

// mimeByExt đoán MIME từ phần mở rộng (khớp contentTypeByExt bên webdav).
func mimeByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".heic", ".heif":
		return "image/heic"
	case ".webp":
		return "image/webp"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".3gp":
		return "video/3gpp"
	default:
		return "application/octet-stream"
	}
}

// ensureThumb sinh thumbnail cho rel vào cache (nếu chưa có) và ghi kích thước
// hiển thị vào DB khi DB chưa biết. background=true chạy chương trình ngoài với
// nice 19 để nhường CPU cho upload và cho người đang xem.
func (d *Dashboard) ensureThumb(ctx context.Context, rel, srcAbs, mime string, fi os.FileInfo, background bool) error {
	cacheRel := thumbCacheRel(rel, fi)
	cacheAbs := filepath.Join(d.store.Root(), filepath.FromSlash(cacheRel))
	if st, err := os.Stat(cacheAbs); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cacheAbs), 0o755); err != nil {
		return err
	}

	// Tên tạm duy nhất: request xem và bộ sinh nền có thể cùng làm một file.
	var rb [6]byte
	_, _ = rand.Read(rb[:])
	tmpAbs := strings.TrimSuffix(cacheAbs, ".jpg") + "." + hex.EncodeToString(rb[:]) + ".tmp.jpg"
	defer os.Remove(tmpAbs) // không còn gì để xoá nếu đã rename thành công

	err := d.generate(ctx, rel, srcAbs, mime, tmpAbs, background)
	if err == nil {
		err = validJPEG(tmpAbs)
	}
	if err != nil {
		// Đánh dấu "không tạo được" để bộ sinh nền không thử mãi — trừ khi lỗi
		// chỉ do bị huỷ (người dùng cuộn qua, server tắt).
		if ctx.Err() == nil && !errors.Is(err, errThumbTooLarge) {
			_ = d.pg.SetDimensionsIfUnknown(context.Background(), rel, -1, -1)
		}
		return err
	}

	w, h, err := jpegSize(tmpAbs)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpAbs, cacheAbs); err != nil {
		return err
	}
	if err := d.pg.SetDimensionsIfUnknown(ctx, rel, w, h); err != nil {
		d.logger.Warn("lưu kích thước từ thumbnail lỗi", "rel", rel, "err", err)
	}
	return nil
}

// generate ghi thumbnail JPEG của srcAbs ra dstAbs bằng công cụ phù hợp.
func (d *Dashboard) generate(ctx context.Context, rel, srcAbs, mime, dstAbs string, background bool) error {
	m := strings.ToLower(mime)
	switch {
	case strings.HasPrefix(m, "video/"):
		if d.tools.ffmpeg == "" {
			return errNoThumbnailer
		}
		return d.videoPoster(ctx, srcAbs, dstAbs, background)

	case strings.HasPrefix(m, "image/") && d.tools.vips != "" && !strings.ContainsAny(srcAbs, "[]"):
		// vips hiểu "[...]" trong tên file là tuỳ chọn → tên có ngoặc vuông dùng Go.
		size := fmt.Sprintf("%dx%d>", thumbMaxWidth, thumbHeight) // ">" = chỉ thu nhỏ
		return d.run(ctx, background, d.tools.vips, srcAbs, "-s", size, "-o", dstAbs+"[Q=80,strip]")

	case isThumbnailable(m):
		data, err := d.renderThumb(rel)
		if err != nil {
			return err
		}
		return os.WriteFile(dstAbs, data, 0o644)
	}
	return errNoThumbnailer
}

// videoPoster cắt khung hình ở giây thứ 1 (bỏ màn hình đen đầu clip); video
// ngắn hơn 1 giây thì lấy khung đầu. ffmpeg tự xoay theo metadata của iPhone.
func (d *Dashboard) videoPoster(ctx context.Context, srcAbs, dstAbs string, background bool) error {
	scale := fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease", thumbMaxWidth, thumbHeight)
	var lastErr error
	for _, seek := range []string{"1", "0"} {
		err := d.run(ctx, background, d.tools.ffmpeg,
			"-nostdin", "-v", "error", "-ss", seek, "-i", srcAbs,
			"-frames:v", "1", "-vf", scale, "-q:v", "4", "-y", dstAbs)
		if err == nil {
			if st, serr := os.Stat(dstAbs); serr == nil && st.Size() > 0 {
				return nil
			}
			err = errors.New("ffmpeg không xuất được khung hình")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	return lastErr
}

// run chạy chương trình ngoài; bị huỷ theo ctx. background → nice 19.
func (d *Dashboard) run(ctx context.Context, background bool, bin string, args ...string) error {
	if background && d.tools.nice != "" {
		args = append([]string{"-n", "19", bin}, args...)
		bin = d.tools.nice
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("%s: %w (%s)", filepath.Base(bin), err, msg)
	}
	return nil
}

func validJPEG(path string) error {
	_, _, err := jpegSize(path)
	return err
}

func jpegSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("thumbnail không hợp lệ: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, errors.New("thumbnail rỗng")
	}
	return cfg.Width, cfg.Height, nil
}

// ---- Bộ sinh thumbnail nền ----

// StartWarmer chạy nền: sinh sẵn thumbnail cho mọi ảnh/video chưa có, mới nhất
// trước, rồi quét lại định kỳ để bắt ảnh mới upload. Nhờ vậy mở dashboard hầu
// như chỉ đọc cache. Chạy tuần tự từng file với nice 19, và tạm nhường khi có
// người đang xem cần thumbnail.
func (d *Dashboard) StartWarmer(ctx context.Context) {
	go func() {
		for {
			start := time.Now()
			if n := d.warmPass(ctx); n > 0 {
				d.logger.Info("đã sinh sẵn thumbnail", "so_file", n, "thoi_gian", time.Since(start).Round(time.Second).String())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(warmRescanPeriod):
			}
		}
	}()
}

func (d *Dashboard) warmPass(ctx context.Context) int {
	made := 0
	var after *store.PageCursor
	for {
		photos, err := d.pg.ListPage(ctx, after, nil, nil, warmPageSize)
		if err != nil {
			if ctx.Err() == nil {
				d.logger.Warn("bộ sinh thumbnail: truy vấn lỗi", "err", err)
			}
			return made
		}
		if len(photos) == 0 {
			return made
		}
		for _, p := range photos {
			if ctx.Err() != nil {
				return made
			}
			if p.Width < 0 || !d.canThumb(p.MIME) {
				continue // đã biết là không tạo được / định dạng không hỗ trợ
			}
			fi, abs, err := d.store.Stat(p.RelPath)
			if err != nil || fi.IsDir() {
				continue
			}
			if d.hasThumb(p.RelPath, fi) {
				continue
			}
			d.waitInteractiveIdle(ctx)
			ictx, cancel := context.WithTimeout(ctx, warmItemTimeout)
			err = d.ensureThumb(ictx, p.RelPath, abs, p.MIME, fi, true)
			cancel()
			if err == nil {
				made++
			} else if ctx.Err() == nil {
				d.logger.Debug("bộ sinh thumbnail: bỏ qua file", "rel", p.RelPath, "err", err)
			}
		}
		last := photos[len(photos)-1]
		after = &store.PageCursor{TakenAt: last.TakenAt, ID: last.ID}
	}
}

func (d *Dashboard) hasThumb(rel string, fi os.FileInfo) bool {
	st, err := os.Stat(filepath.Join(d.store.Root(), filepath.FromSlash(thumbCacheRel(rel, fi))))
	return err == nil && st.Size() > 0
}

// waitInteractiveIdle chờ khi đang có request /thumb của người xem chạy, để bộ
// sinh nền không tranh CPU/đĩa với thứ người dùng đang nhìn.
func (d *Dashboard) waitInteractiveIdle(ctx context.Context) {
	for len(d.thumbSem) > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}
