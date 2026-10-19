// Command reindex quét thư mục dữ liệu và dựng lại bảng Postgres từ chính các file.
// Dùng khi mất/đổi Postgres: file-trên-đĩa là source of truth.
//
//	go run ./cmd/reindex
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/integrity"
	"github.com/local/photovault/internal/store"
)

func main() {
	envPath := flag.String("env", ".env", "đường dẫn file .env")
	flag.Parse()

	cfg, err := config.Load(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nạp cấu hình:", err)
		os.Exit(1)
	}
	if cfg.PGDSN == "" {
		fmt.Fprintln(os.Stderr, "reindex cần NCFAKE_PG_DSN trong .env")
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, cfg.PGDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kết nối Postgres:", err)
		os.Exit(1)
	}
	defer pg.Close()

	rep, err := integrity.Reindex(ctx, cfg.DataDir, pg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reindex lỗi:", err)
		os.Exit(1)
	}
	fmt.Printf("Đã index: %d file | Xoá bản ghi cũ: %d\n", rep.Indexed, rep.Removed)
}
