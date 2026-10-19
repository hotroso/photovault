// Package storage ghi ảnh/video RAW xuống đĩa với hai bảo đảm toàn vẹn cốt lõi
// (DESIGN.md §6b):
//   - Ghi atomic: ghi ra *.part rồi fsync rồi rename → không bao giờ để file ghi dở.
//   - Checksum khi ghi: tính SHA-256 trong lúc stream, không đọc lại file.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Storage quản lý một thư mục gốc chứa dữ liệu raw.
type Storage struct {
	root string
}

// New tạo Storage với thư mục gốc (DataDir).
func New(root string) *Storage {
	return &Storage{root: root}
}

// Result là kết quả ghi một file.
type Result struct {
	RelPath string
	AbsPath string
	Size    int64
	SHA256  string
}

// safeJoin ghép root với relPath đã làm sạch, chống path traversal (../).
func (s *Storage) safeJoin(relPath string) (string, error) {
	clean := filepath.Clean("/" + strings.ReplaceAll(relPath, "\\", "/"))
	clean = strings.TrimPrefix(clean, "/")
	abs := filepath.Join(s.root, filepath.FromSlash(clean))
	// Đảm bảo vẫn nằm trong root.
	rootAbs, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	fileAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(fileAbs, rootAbs) {
		return "", fmt.Errorf("đường dẫn thoát khỏi thư mục gốc: %s", relPath)
	}
	return abs, nil
}

// WriteAtomic ghi dữ liệu từ src vào relPath theo kiểu atomic, tính SHA-256.
// Nếu expectedSize >= 0 và kích thước thực khác → lỗi, không rename.
// mtime (nếu không zero) được áp vào file sau khi ghi (giữ ngày chụp).
func (s *Storage) WriteAtomic(relPath string, src io.Reader, expectedSize int64, mtime time.Time) (*Result, error) {
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("tạo thư mục: %w", err)
	}

	tmp := abs + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mở file tạm: %w", err)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)

	n, copyErr := io.Copy(mw, src)
	if copyErr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("ghi dữ liệu: %w", copyErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("fsync: %w", syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("đóng file tạm: %w", closeErr)
	}

	if expectedSize >= 0 && n != expectedSize {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("kích thước không khớp: nhận %d, mong đợi %d", n, expectedSize)
	}

	if err := os.Rename(tmp, abs); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("rename atomic: %w", err)
	}

	if !mtime.IsZero() {
		_ = os.Chtimes(abs, mtime, mtime)
	}

	return &Result{
		RelPath: filepath.ToSlash(relPathFromRoot(s.root, abs)),
		AbsPath: abs,
		Size:    n,
		SHA256:  hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// Mkdir tạo một thư mục (cho WebDAV MKCOL).
func (s *Storage) Mkdir(relPath string) error {
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return err
	}
	return os.MkdirAll(abs, 0o755)
}

// Stat trả thông tin file/thư mục.
func (s *Storage) Stat(relPath string) (os.FileInfo, string, error) {
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return nil, "", err
	}
	fi, err := os.Stat(abs)
	return fi, abs, err
}

// Remove xoá file hoặc cây thư mục.
func (s *Storage) Remove(relPath string) error {
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return err
	}
	return os.RemoveAll(abs)
}

// Open mở file để đọc (dùng cho dashboard/thumbnail).
func (s *Storage) Open(relPath string) (*os.File, error) {
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return nil, err
	}
	return os.Open(abs)
}

// Root trả thư mục gốc tuyệt đối.
func (s *Storage) Root() string { return s.root }

// relPathFromRoot tính đường dẫn tương đối từ root tới abs.
func relPathFromRoot(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}
