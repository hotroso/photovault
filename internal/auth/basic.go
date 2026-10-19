package auth

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/local/photovault/internal/store"
)

// Verifier kiểm tra app password. Tách interface để dễ test và để phần WebDAV
// không phụ thuộc trực tiếp vào Redis.
type Verifier struct {
	redis *store.Redis
	user  string
}

// NewVerifier tạo verifier dựa trên Redis store và username hợp lệ duy nhất.
func NewVerifier(r *store.Redis, user string) *Verifier {
	return &Verifier{redis: r, user: user}
}

// RequireBasicAuth bọc handler WebDAV: chỉ cho qua khi Basic Auth mang app
// password hợp lệ (đã cấp qua Login Flow v2 hoặc getapppassword).
func (v *Verifier) RequireBasicAuth(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		if !v.valid(r.Context(), user, pass) {
			logger.Warn("từ chối WebDAV: sai thông tin xác thực", "user", user, "path", r.URL.Path)
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// valid trả true nếu (user, pass) khớp một app password hợp lệ.
func (v *Verifier) valid(ctx context.Context, user, pass string) bool {
	if user != v.user {
		return false
	}
	loginName, ok, err := v.redis.CheckAppPassword(ctx, pass)
	if err != nil || !ok {
		return false
	}
	return loginName == v.user
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="Nextcloud"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}
