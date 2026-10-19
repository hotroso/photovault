package compat

import (
	"log/slog"
	"net/http"

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
	ID          string `json:"id"`
	DisplayName string `json:"display-name"`
	DisplayName2 string `json:"displayname"`
	Enabled     bool   `json:"enabled"`
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
