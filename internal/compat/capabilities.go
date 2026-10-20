package compat

import (
	"log/slog"
	"net/http"
	"syscall"

	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/httpx"
)

// Capabilities xử lý GET /ocs/v1.php|v2.php/cloud/capabilities.
//
// App dùng response này để biết server hỗ trợ gì. Tối thiểu cần khối `core`
// (webdav-root) và `files` (bật bigfilechunking để app dùng chunked upload v2).
func Capabilities(cfg *config.Config, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"version": map[string]any{
				"major":           28,
				"minor":           0,
				"micro":           0,
				"string":          cfg.NCVersionString,
				"edition":         "",
				"extendedSupport": false,
			},
			"capabilities": map[string]any{
				"core": map[string]any{
					"pollinterval": 60,
					"webdav-root":  "remote.php/webdav",
				},
				"files": map[string]any{
					"bigfilechunking": true,
					"chunked_upload": map[string]any{
						// 100 MiB mỗi chunk; app tự chia nhỏ file lớn.
						"max_size":           104857600,
						"max_parallel_count": 5,
					},
				},
				"dav": map[string]any{
					"chunking": "1.0",
				},
			},
		}
		httpx.OCS(w, logger, data)
	}
}

// userPayload là data của endpoint /cloud/user.
type userPayload struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display-name"`
	DisplayName2 string `json:"displayname"`
	Enabled      bool   `json:"enabled"`
}

// userDetail là data của /cloud/users/{id} — app dùng cho màn hình tài khoản
// (tên hiển thị, dung lượng).
type userDetail struct {
	ID          string    `json:"id"`
	Enabled     bool      `json:"enabled"`
	DisplayName string    `json:"displayname"`
	Email       string    `json:"email"`
	Quota       userQuota `json:"quota"`
	Groups      []string  `json:"groups"`
	Language    string    `json:"language"`
	Locale      string    `json:"locale"`
	Backend     string    `json:"backend"`
}

type userQuota struct {
	Free     int64   `json:"free"`
	Used     int64   `json:"used"`
	Total    int64   `json:"total"`
	Relative float64 `json:"relative"`
	Quota    int64   `json:"quota"` // -3 = không giới hạn (theo Nextcloud)
}

// UserDetail xử lý GET /ocs/v1.php|v2.php/cloud/users/{userid}. Dung lượng lấy
// từ ổ đĩa chứa APP_DATA_DIR.
func UserDetail(cfg *config.Config, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("userid") != cfg.User {
			http.NotFound(w, r)
			return
		}
		q := userQuota{Quota: -3}
		var st syscall.Statfs_t
		if err := syscall.Statfs(cfg.DataDir, &st); err == nil {
			q.Total = int64(st.Blocks) * st.Bsize
			q.Free = int64(st.Bavail) * st.Bsize
			q.Used = q.Total - q.Free
			if q.Total > 0 {
				q.Relative = float64(q.Used) * 100 / float64(q.Total)
			}
		}
		httpx.OCS(w, logger, userDetail{
			ID:          cfg.User,
			Enabled:     true,
			DisplayName: cfg.DisplayName,
			Quota:       q,
			Groups:      []string{},
			Language:    "vi",
			Locale:      "vi_VN",
			Backend:     "Database",
		})
	}
}

// User xử lý GET /ocs/v1.php|v2.php/cloud/user — app hỏi để biết username phục vụ
// việc dựng URL WebDAV (/remote.php/dav/files/<user>/...).
func User(cfg *config.Config, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.OCS(w, logger, userPayload{
			ID:           cfg.User,
			DisplayName:  cfg.DisplayName,
			DisplayName2: cfg.DisplayName,
			Enabled:      true,
		})
	}
}
