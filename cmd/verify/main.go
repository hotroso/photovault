// Command verify quét thư mục dữ liệu, so SHA-256 với Postgres, báo file hỏng/lệch.
//
//	go run ./cmd/verify
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
		fmt.Fprintln(os.Stderr, "verify cần NCFAKE_PG_DSN trong .env")
		os.Exit(1)
	}

	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, cfg.PGDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kết nối Postgres:", err)
		os.Exit(1)
	}
	defer pg.Close()

	rep, err := integrity.Verify(ctx, cfg.DataDir, pg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify lỗi:", err)
		os.Exit(1)
	}

	fmt.Printf("Đã kiểm tra: %d | OK: %d\n", rep.Checked, rep.OK)
	printList("LỆCH CHECKSUM (nghi hỏng/bit rot)", rep.Mismatched)
	printList("CÓ TRÊN ĐĨA, THIẾU TRONG DB", rep.MissingInDB)
	printList("CÓ TRONG DB, MẤT TRÊN ĐĨA", rep.MissingOnFS)

	if len(rep.Mismatched) > 0 || len(rep.MissingOnFS) > 0 {
		os.Exit(2) // có vấn đề toàn vẹn
	}
}

func printList(title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Printf("\n== %s (%d) ==\n", title, len(items))
	for _, it := range items {
		fmt.Println("  -", it)
	}
}
