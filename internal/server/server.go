package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Run khởi động HTTP(S) server và chặn cho tới khi nhận tín hiệu dừng (Ctrl+C /
// SIGTERM), sau đó shutdown mềm.
func Run(d Deps) error {
	cfg, logger := d.Cfg, d.Logger
	handler := NewRouter(d)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		// Không đặt WriteTimeout chặt: upload ảnh/video lớn có thể lâu.
	}

	errCh := make(chan error, 1)
	go func() {
		if cfg.UsesTLS() {
			logger.Info("server lắng nghe (HTTPS)", "addr", cfg.ListenAddr, "public_url", cfg.PublicURL)
			errCh <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			logger.Info("server lắng nghe (HTTP)", "addr", cfg.ListenAddr, "public_url", cfg.PublicURL)
			errCh <- srv.ListenAndServe()
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case sig := <-stop:
		logger.Info("nhận tín hiệu dừng, đang shutdown", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
