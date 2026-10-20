package webdav

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/local/photovault/internal/storage"
	"github.com/local/photovault/internal/store"
)

// TestStoreWithDedup kiểm tra luồng stage → tra hash toàn cục → finalize:
// lần upload đầu của một chuỗi byte rơi vào photos/<yyyymm>/, lần thứ hai (cùng
// byte, tên khác) rơi vào recyclebin/<yyyymm>/ với dup=true và dup_of trỏ bản gốc.
//
// Cần một Postgres dùng-rồi-bỏ qua APP_PG_DSN; không có thì t.Skip.
func TestStoreWithDedup(t *testing.T) {
	dsn := os.Getenv("APP_PG_DSN")
	if dsn == "" {
		t.Skip("APP_PG_DSN chưa đặt → bỏ qua kiểm tra dedup cần Postgres")
	}

	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("mở Postgres: %v", err)
	}
	defer pg.Close()

	dataDir := t.TempDir()
	st := storage.New(dataDir)
	h := &Handler{
		store:  st,
		pg:     pg,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Byte duy nhất cho lần chạy này để không đụng dữ liệu cũ trong DB.
	payload := []byte("photovault dedup test " + time.Now().Format(time.RFC3339Nano))
	mtime := time.Date(2023, 3, 15, 10, 0, 0, 0, time.UTC)

	// Lần 1: bản gốc → photos/202303/
	staged1, err := st.StageTemp(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("StageTemp lần 1: %v", err)
	}
	res1, dup1, _, err := h.storeWithDedup(ctx, staged1, "IMG_0001.JPG", mtime, "me/Photos/IMG_0001.JPG")
	if err != nil {
		t.Fatalf("storeWithDedup lần 1: %v", err)
	}
	if dup1 {
		t.Fatalf("lần 1 không được coi là trùng")
	}
	if !strings.HasPrefix(res1.RelPath, "photos/202303/") {
		t.Fatalf("bản gốc phải nằm dưới photos/202303/, có: %s", res1.RelPath)
	}

	orig, err := pg.GetBySHA256(ctx, res1.SHA256)
	if err != nil || orig == nil {
		t.Fatalf("không tìm thấy bản gốc trong index: %v", err)
	}

	// Lần 2: cùng byte, tên khác → recyclebin/202303/, dup=true, dup_of=orig.ID
	staged2, err := st.StageTemp(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("StageTemp lần 2: %v", err)
	}
	res2, dup2, _, err := h.storeWithDedup(ctx, staged2, "IMG_0002.JPG", mtime, "me/Photos/IMG_0002.JPG")
	if err != nil {
		t.Fatalf("storeWithDedup lần 2: %v", err)
	}
	if !dup2 {
		t.Fatalf("lần 2 phải được coi là trùng")
	}
	if !strings.HasPrefix(res2.RelPath, "recyclebin/202303/") {
		t.Fatalf("bản trùng phải nằm dưới recyclebin/202303/, có: %s", res2.RelPath)
	}
	if res2.SHA256 != res1.SHA256 {
		t.Fatalf("hai lần phải cùng hash: %s vs %s", res1.SHA256, res2.SHA256)
	}

	dupRow, err := pg.GetByRelPath(ctx, res2.RelPath)
	if err != nil || dupRow == nil {
		t.Fatalf("không tìm thấy bản trùng trong index: %v", err)
	}
	if !dupRow.IsTrash {
		t.Fatalf("bản trùng phải có is_trash=true")
	}
	if dupRow.DupOf == nil || *dupRow.DupOf != orig.ID {
		t.Fatalf("dup_of phải trỏ về bản gốc id=%d, có: %v", orig.ID, dupRow.DupOf)
	}

	// Dọn các bản ghi test khỏi DB dùng-rồi-bỏ.
	_ = pg.DeletePhotoByRelPath(ctx, res2.RelPath)
	_ = pg.DeletePhotoByRelPath(ctx, res1.RelPath)
}
