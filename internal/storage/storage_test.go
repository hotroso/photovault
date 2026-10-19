package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestStageTemp xác nhận StageTemp trả đúng SHA-256 + Size và để lại file dưới .staging/.
func TestStageTemp(t *testing.T) {
	s := New(t.TempDir())
	data := []byte("xin chào photovault")

	res, err := s.StageTemp(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("StageTemp lỗi: %v", err)
	}
	if res.Size != int64(len(data)) {
		t.Errorf("Size = %d, mong đợi %d", res.Size, len(data))
	}
	if want := sha256Hex(data); res.SHA256 != want {
		t.Errorf("SHA256 = %s, mong đợi %s", res.SHA256, want)
	}
	if dir := filepath.Dir(res.TempRel); dir != ".staging" {
		t.Errorf("TempRel = %q, mong đợi nằm trong .staging/", res.TempRel)
	}
	abs := filepath.Join(s.Root(), res.TempRel)
	if _, err := os.Stat(abs); err != nil {
		t.Errorf("file staging không tồn tại: %v", err)
	}
}

// TestStageTempSizeMismatch xác nhận kích thước sai thì xoá file tạm và báo lỗi.
func TestStageTempSizeMismatch(t *testing.T) {
	s := New(t.TempDir())
	data := []byte("abc")
	if _, err := s.StageTemp(bytes.NewReader(data), 999); err == nil {
		t.Fatal("mong đợi lỗi kích thước không khớp, nhận nil")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), ".staging"))
	if len(entries) != 0 {
		t.Errorf("file tạm chưa được dọn: còn %d mục", len(entries))
	}
}

// TestFinalizeStagedLayout xác nhận bytes được đặt tại photos/<yyyymm>/<base>.ext theo mtime.
func TestFinalizeStagedLayout(t *testing.T) {
	s := New(t.TempDir())
	data := []byte("ngày gốc tháng ba")
	mtime := time.Date(2023, 3, 15, 10, 0, 0, 0, time.UTC)

	st, err := s.StageTemp(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("StageTemp lỗi: %v", err)
	}
	res, err := s.FinalizeStaged(st.TempRel, "photos", "IMG_0001.HEIC", st.SHA256, mtime)
	if err != nil {
		t.Fatalf("FinalizeStaged lỗi: %v", err)
	}
	want := "photos/202303/IMG_0001.HEIC"
	if res.RelPath != want {
		t.Errorf("RelPath = %q, mong đợi %q", res.RelPath, want)
	}
	got, err := os.ReadFile(res.AbsPath)
	if err != nil {
		t.Fatalf("đọc file đích lỗi: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("nội dung file đích không khớp")
	}
}

// TestFinalizeStagedCollisionDifferent xác nhận bytes KHÁC cùng tên gốc → _<hash8> và không ghi đè.
func TestFinalizeStagedCollisionDifferent(t *testing.T) {
	s := New(t.TempDir())
	mtime := time.Date(2023, 3, 15, 10, 0, 0, 0, time.UTC)

	d1 := []byte("file một")
	st1, _ := s.StageTemp(bytes.NewReader(d1), int64(len(d1)))
	r1, err := s.FinalizeStaged(st1.TempRel, "photos", "IMG.JPG", st1.SHA256, mtime)
	if err != nil {
		t.Fatalf("finalize 1 lỗi: %v", err)
	}

	d2 := []byte("file hai khác hẳn")
	st2, _ := s.StageTemp(bytes.NewReader(d2), int64(len(d2)))
	r2, err := s.FinalizeStaged(st2.TempRel, "photos", "IMG.JPG", st2.SHA256, mtime)
	if err != nil {
		t.Fatalf("finalize 2 lỗi: %v", err)
	}

	if r1.RelPath == r2.RelPath {
		t.Fatalf("tên trùng không được tách: cả hai là %q", r1.RelPath)
	}
	want := "photos/202303/IMG_" + st2.SHA256[:8] + ".JPG"
	if r2.RelPath != want {
		t.Errorf("RelPath bản 2 = %q, mong đợi %q", r2.RelPath, want)
	}
	// Bản gốc không bị ghi đè.
	got1, _ := os.ReadFile(r1.AbsPath)
	if !bytes.Equal(got1, d1) {
		t.Errorf("file gốc bị thay đổi")
	}
}

// TestFinalizeStagedIdempotent xác nhận bytes GIỐNG cùng tên gốc không tạo file thứ hai.
func TestFinalizeStagedIdempotent(t *testing.T) {
	s := New(t.TempDir())
	mtime := time.Date(2023, 3, 15, 10, 0, 0, 0, time.UTC)
	data := []byte("cùng một nội dung")

	st1, _ := s.StageTemp(bytes.NewReader(data), int64(len(data)))
	r1, err := s.FinalizeStaged(st1.TempRel, "photos", "IMG.JPG", st1.SHA256, mtime)
	if err != nil {
		t.Fatalf("finalize 1 lỗi: %v", err)
	}
	st2, _ := s.StageTemp(bytes.NewReader(data), int64(len(data)))
	r2, err := s.FinalizeStaged(st2.TempRel, "photos", "IMG.JPG", st2.SHA256, mtime)
	if err != nil {
		t.Fatalf("finalize 2 lỗi: %v", err)
	}
	if r1.RelPath != r2.RelPath {
		t.Errorf("không idempotent: %q vs %q", r1.RelPath, r2.RelPath)
	}
	entries, _ := os.ReadDir(filepath.Dir(r1.AbsPath))
	if len(entries) != 1 {
		t.Errorf("mong đợi 1 file, có %d", len(entries))
	}
	// Staging của lần 2 đã được dọn.
	stag, _ := os.ReadDir(filepath.Join(s.Root(), ".staging"))
	if len(stag) != 0 {
		t.Errorf("staging chưa dọn sau idempotent: %d mục", len(stag))
	}
}

// TestFinalizeStagedTraversalRejected xác nhận tên gốc cố path traversal bị safeJoin chặn/làm sạch.
func TestFinalizeStagedTraversalRejected(t *testing.T) {
	s := New(t.TempDir())
	data := []byte("x")
	st, _ := s.StageTemp(bytes.NewReader(data), int64(len(data)))

	res, err := s.FinalizeStaged(st.TempRel, "photos", "../../etc/passwd", st.SHA256, time.Time{})
	if err != nil {
		return // bị từ chối là chấp nhận được
	}
	// Nếu không lỗi, đích phải vẫn nằm trong root (đã được làm sạch).
	rootAbs, _ := filepath.Abs(s.Root())
	absClean, _ := filepath.Abs(res.AbsPath)
	rel, relErr := filepath.Rel(rootAbs, absClean)
	if relErr != nil || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		t.Fatalf("đích thoát khỏi root: %q", res.AbsPath)
	}
}
