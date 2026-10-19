// Package httpx chứa middleware và tiện ích HTTP dùng chung.
package httpx

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder bọc ResponseWriter để bắt status code và số byte đã ghi.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// LogRequests ghi log mọi request đến. Đây là công cụ debug handshake quan trọng
// nhất: khi app iOS từ chối, log cho biết chính xác nó gọi method/path/header gì
// và server trả về status nào, theo đúng thứ tự.
func LogRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", rec.status,
			"bytes", rec.bytes,
			"dur_ms", time.Since(start).Milliseconds(),
			"ua", r.UserAgent(),
			"remote", r.RemoteAddr,
			"ocs_apirequest", r.Header.Get("OCS-APIRequest"),
		)
	})
}
