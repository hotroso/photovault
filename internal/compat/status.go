// Package compat cài các endpoint giả lập Nextcloud mà app iOS yêu cầu trước khi
// cho phép đồng bộ (handshake). Định dạng bám theo source Nextcloud thật trong
// reference/nextcloud-server/.
package compat

import (
	"log/slog"
	"net/http"

	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/httpx"
)

// statusPayload khớp đúng 8 field mà status.php thật trả về
// (xem reference/nextcloud-server/server-master/status.php).
type statusPayload struct {
	Installed       bool   `json:"installed"`
	Maintenance     bool   `json:"maintenance"`
	NeedsDBUpgrade  bool   `json:"needsDbUpgrade"`
	Version         string `json:"version"`
	VersionString   string `json:"versionstring"`
	Edition         string `json:"edition"`
	ProductName     string `json:"productname"`
	ExtendedSupport bool   `json:"extendedSupport"`
}

// Status xử lý GET /status.php — bước đầu tiên app gọi để nhận diện "đây là Nextcloud".
func Status(cfg *config.Config, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Nextcloud thật đặt CORS mở trên status.php.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		httpx.JSON(w, logger, http.StatusOK, statusPayload{
			Installed:       true,
			Maintenance:     false,
			NeedsDBUpgrade:  false,
			Version:         cfg.NCVersion,
			VersionString:   cfg.NCVersionString,
			Edition:         "",
			ProductName:     "Nextcloud",
			ExtendedSupport: false,
		})
	}
}
