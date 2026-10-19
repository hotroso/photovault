package webdav

import (
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// parseOCMtime đọc header X-OC-Mtime (unixtime, giây) → time.Time.
// Trả zero nếu rỗng/không hợp lệ.
func parseOCMtime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// originalDate xác định NGÀY GỐC của ảnh để chọn thư mục yyyymm và taken_at,
// theo thứ tự ưu tiên:
//  1. X-OC-Mtime (nếu app gửi và hợp lệ) — chính xác nhất.
//  2. Ngày suy từ TÊN FILE do app Nextcloud iOS đặt: "YY-MM-DD HH-MM-SS ...".
//     Thực tế app này KHÔNG gửi X-OC-Mtime nên tên file là nguồn chính.
//  3. Zero (caller sẽ fallback time.Now()).
func originalDate(ocMtime time.Time, name string) time.Time {
	if !ocMtime.IsZero() {
		return ocMtime
	}
	if t, ok := parseDateFromName(name); ok {
		return t
	}
	return time.Time{}
}

// parseDateFromName rút ngày từ tên file dạng "YY-MM-DD HH-MM-SS ...".
// Ví dụ "24-06-06 12-11-19 1668.jpg" → 2024-06-06 12:11:19 (giờ địa phương).
// YY 00–69 coi là 20xx, 70–99 là 19xx (quy ước thế kỷ thông dụng). Trả ok=false
// nếu không khớp hoặc ngày/giờ không hợp lệ.
func parseDateFromName(name string) (time.Time, bool) {
	// Chỉ cần tiền tố đủ dài "YY-MM-DD HH-MM-SS" (17 ký tự).
	const layout = "06-01-02 15-04-05"
	base := path.Base(name)
	if len(base) < len(layout) {
		return time.Time{}, false
	}
	prefix := base[:len(layout)]
	t, err := time.ParseInLocation(layout, prefix, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	// Loại giá trị vô lý (vd năm quá xa tương lai do tên rác).
	if t.Year() < 1990 || t.Year() > time.Now().Year()+1 {
		return time.Time{}, false
	}
	return t, true
}

// destRelFromHeader tách đường dẫn tương đối (sau /remote.php/dav/files/<user>)
// từ header Destination (là URL tuyệt đối hoặc path). Giữ phần "<user>/...".
func destRelFromHeader(dest string) string {
	if dest == "" {
		return ""
	}
	// Destination có thể là URL tuyệt đối.
	if u, err := url.Parse(dest); err == nil && u.Path != "" {
		dest = u.Path
	}
	dest = strings.TrimPrefix(dest, "/remote.php/dav/files/")
	dest = strings.TrimPrefix(dest, "/remote.php/dav/uploads/")
	return strings.Trim(dest, "/")
}

// contentTypeByExt đoán MIME từ phần mở rộng, phủ các định dạng ảnh/video iPhone.
func contentTypeByExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".heic", ".heif":
		return "image/heic"
	case ".webp":
		return "image/webp"
	case ".dng":
		return "image/x-adobe-dng"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}
