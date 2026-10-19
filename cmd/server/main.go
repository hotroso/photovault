// Command server khởi động Go fake Nextcloud server.
//
// Cách chạy:
//
//	cp .env.example .env   # rồi điền giá trị
//	go run ./cmd/server
//
// Cờ:
//
//	-env   đường dẫn file .env (mặc định ".env")
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/server"
	"github.com/local/photovault/internal/storage"
	"github.com/local/photovault/internal/store"
)

func main() {
	envPath := flag.String("env", ".env", "đường dẫn file .env")
	flag.Parse()

	cfg, err := config.Load(*envPath)
	if err != nil {
		slog.Error("nạp cấu hình thất bại", "err", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		logger.Error("tạo thư mục dữ liệu thất bại", "dir", cfg.DataDir, "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	pg, err := store.NewPostgres(ctx, cfg.PGDSN)
	if err != nil {
		logger.Error("kết nối Postgres thất bại", "err", err)
		os.Exit(1)
	}
	defer pg.Close()

	rd, err := store.NewRedis(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.RedisPrefix)
	if err != nil {
		logger.Error("kết nối Redis thất bại", "err", err)
		os.Exit(1)
	}
	defer rd.Close()

	st := storage.New(cfg.DataDir)

	logger.Info("khởi động photovault",
		"user", cfg.User,
		"data_dir", cfg.DataDir,
		"tls", cfg.UsesTLS(),
		"pg", cfg.PGDSN != "",
		"redis", cfg.RedisAddr != "",
	)

	deps := server.Deps{
		Cfg:     cfg,
		Logger:  logger,
		Storage: st,
		PG:      pg,
		Redis:   rd,
	}
	if err := server.Run(deps); err != nil {
		logger.Error("server dừng với lỗi", "err", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}
