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
