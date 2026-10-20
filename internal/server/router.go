// Package server lắp ráp router và vòng đời HTTP server.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/local/photovault/internal/auth"
	"github.com/local/photovault/internal/compat"
	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/dashboard"
	"github.com/local/photovault/internal/httpx"
	"github.com/local/photovault/internal/storage"
	"github.com/local/photovault/internal/store"
	"github.com/local/photovault/internal/webdav"
)

// Deps gom các phụ thuộc runtime cho router.
type Deps struct {
	Cfg     *config.Config
	Logger  *slog.Logger
	Storage *storage.Storage
	PG      *store.Postgres
	Redis   *store.Redis
}

// NewRouter dựng toàn bộ route và trả về handler đã bọc logging.
func NewRouter(d Deps) http.Handler {
	cfg, logger := d.Cfg, d.Logger
	mux := http.NewServeMux()

	// ---- M0: phát hiện server ----
	mux.HandleFunc("GET /status.php", compat.Status(cfg, logger))

	// ---- M1: capabilities + user (cả v1.php và v2.php) ----
	mux.HandleFunc("GET /ocs/v1.php/cloud/capabilities", compat.Capabilities(cfg, logger))
	mux.HandleFunc("GET /ocs/v2.php/cloud/capabilities", compat.Capabilities(cfg, logger))
	mux.HandleFunc("GET /ocs/v1.php/cloud/user", compat.User(cfg, logger))
	mux.HandleFunc("GET /ocs/v2.php/cloud/user", compat.User(cfg, logger))
	mux.HandleFunc("GET /ocs/v1.php/cloud/users/{userid}", compat.UserDetail(cfg, logger))
	mux.HandleFunc("GET /ocs/v2.php/cloud/users/{userid}", compat.UserDetail(cfg, logger))

	// ---- M2: Login Flow v2 ----
	lf := compat.NewLoginFlow(cfg, d.Redis, logger)
	mux.HandleFunc("POST /index.php/login/v2", lf.Init)
	mux.HandleFunc("POST /login/v2", lf.Init)
	mux.HandleFunc("POST /login/v2/poll", lf.Poll)
	mux.HandleFunc("GET /login/v2/flow/{token}", lf.Landing)
	mux.HandleFunc("POST /login/v2/grant", lf.Grant)
	mux.HandleFunc("GET /ocs/v2.php/core/getapppassword", lf.GetAppPassword)

	// ---- M3–M4: WebDAV (yêu cầu Basic Auth với app password) ----
	dav := webdav.NewHandler(cfg, d.Storage, d.PG, logger)
	verifier := auth.NewVerifier(d.Redis, cfg.User)

	davFiles := http.HandlerFunc(dav.ServeFiles)
	davUploads := http.HandlerFunc(dav.ServeUploads)
	// ServeMux của Go 1.22 định tuyến theo method+pattern, nhưng WebDAV dùng các
	// method lạ (PROPFIND/MKCOL/MOVE) nên ta nhận mọi method qua pattern prefix và
	// tự điều phối bên trong handler.
	mux.Handle("/remote.php/dav/files/", verifier.RequireBasicAuth(logger, davFiles))
	mux.Handle("/remote.php/dav/uploads/", verifier.RequireBasicAuth(logger, davUploads))
	// Một số client dùng /remote.php/webdav (webdav-root cũ) → ánh xạ sang files.
	mux.Handle("/remote.php/webdav/", verifier.RequireBasicAuth(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/remote.php/dav/files/" + cfg.User + strings.TrimPrefix(r.URL.Path, "/remote.php/webdav")
		dav.ServeFiles(w, r)
	})))

	// ---- M5: Dashboard ----
	dash := dashboard.New(cfg, d.Storage, d.PG, logger)
	// Sinh sẵn thumbnail nền (nice 19) để mở dashboard hầu như chỉ đọc cache.
	dash.StartWarmer(context.Background())

	// ---- M6: xem ảnh trong app (tab Media + lưới Files) ----
	// SEARCH liệt kê media; core/preview trả thumbnail theo fileId (dùng chung
	// cache thumbnail với dashboard). Cả hai cần app password như WebDAV.
	dav.SetPreviewSupport(dash.CanThumb)
	search := verifier.RequireBasicAuth(logger, http.HandlerFunc(dav.ServeSearch))
	mux.Handle("/remote.php/dav", search)
	mux.Handle("/remote.php/dav/{$}", search)
	preview := verifier.RequireBasicAuth(logger, http.HandlerFunc(dash.Preview))
	mux.Handle("GET /index.php/core/preview", preview)
	mux.Handle("GET /core/preview", preview)
	mux.HandleFunc("GET /dashboard", dash.Index)
	mux.HandleFunc("GET /api/photos", dash.APIPhotos)
	mux.Handle("GET /static/", dashboard.StaticHandler())
	mux.HandleFunc("GET /thumb", dash.Thumb)
	mux.HandleFunc("GET /media", dash.Media)
	mux.HandleFunc("POST /trash/delete", dash.TrashDelete)
	mux.HandleFunc("POST /trash/restore", dash.TrashRestore)

	mux.HandleFunc("/", rootHandler(logger))

	return httpx.LogRequests(logger, mux)
}

// rootHandler trả thông tin cơ bản ở "/", 404 cho path chưa cài (vẫn vào log).
func rootHandler(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("photovault: OK. Xem /dashboard\n"))
			return
		}
		http.NotFound(w, r)
	}
}
