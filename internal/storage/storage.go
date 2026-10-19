// Package storage ghi ảnh/video RAW xuống đĩa với hai bảo đảm toàn vẹn cốt lõi
// (DESIGN.md §6b):
//   - Ghi atomic: ghi ra *.part rồi fsync rồi rename → không bao giờ để file ghi dở.
//   - Checksum khi ghi: tính SHA-256 trong lúc stream, không đọc lại file.
package storage

import (
	"crypto/rand"
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

// StageResult là kết quả của bước staging: file đã ghi xuống .staging/, đã biết
// SHA-256, nhưng CHƯA được đặt vào photos/ hay recyclebin/. Caller dùng hash này
// để tra cứu trùng lặp toàn cục trước khi quyết định đích đến.
type StageResult struct {
	TempRel string // đường dẫn tương đối tới file staging, ví dụ ".staging/ab12cd.part"
	Size    int64
	SHA256  string
}

// StageTemp stream dữ liệu từ src vào một file tạm trong .staging/, tính SHA-256
// trong lúc ghi (đọc một lần duy nhất) rồi fsync. KHÔNG rename — đích đến được
// quyết định sau ở FinalizeStaged dựa trên hash. Dùng cho luồng dedup.
// Nếu expectedSize >= 0 và kích thước thực khác → xoá file tạm và trả lỗi.
func (s *Storage) StageTemp(src io.Reader, expectedSize int64) (*StageResult, error) {
	var rb [16]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return nil, fmt.Errorf("sinh tên tạm: %w", err)
	}
	tempRel := filepath.ToSlash(filepath.Join(".staging", hex.EncodeToString(rb[:])+".part"))

	abs, err := s.safeJoin(tempRel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("tạo thư mục staging: %w", err)
	}

	f, err := os.OpenFile(abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mở file staging: %w", err)
	}

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)

	n, copyErr := io.Copy(mw, src)
	if copyErr != nil {
		f.Close()
		_ = os.Remove(abs)
		return nil, fmt.Errorf("ghi dữ liệu staging: %w", copyErr)
	}
	if syncErr := f.Sync(); syncErr != nil {
		f.Close()
		_ = os.Remove(abs)
		return nil, fmt.Errorf("fsync staging: %w", syncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(abs)
		return nil, fmt.Errorf("đóng file staging: %w", closeErr)
	}

	if expectedSize >= 0 && n != expectedSize {
		_ = os.Remove(abs)
		return nil, fmt.Errorf("kích thước không khớp: nhận %d, mong đợi %d", n, expectedSize)
	}

	return &StageResult{
		TempRel: tempRel,
		Size:    n,
		SHA256:  hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

// RemoveStaged xoá một file staging (dùng ở nhánh lỗi của caller).
func (s *Storage) RemoveStaged(tempRel string) error {
	abs, err := s.safeJoin(tempRel)
	if err != nil {
		return err
	}
	return os.Remove(abs)
}

// yyyymmFromTime trả về chuỗi "yyyymm" từ ngày gốc của file. Dùng time.Now()
// khi t rỗng (không có X-OC-Mtime).
func yyyymmFromTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.Format("200601")
}

// collisionSafeName chọn một tên file không ghi đè file khác trong destDirAbs.
//   - Nếu tên còn trống → dùng luôn.
//   - Nếu đã có file cùng tên nhưng nội dung KHÁC → thử "<stem>_<hash8><ext>",
//     rồi "<stem>_<hash12><ext>".
//   - Nếu file cùng tên có nội dung GIỐNG HỆT → tái dùng tên đó (idempotent).
//
// So sánh nội dung rẻ trước: so size, chỉ hash file hiện có khi size khớp.
func (s *Storage) collisionSafeName(destDirAbs, baseName, fullHash string) (string, error) {
	ext := filepath.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)

	candidates := []string{baseName}
	if len(fullHash) >= 8 {
		candidates = append(candidates, stem+"_"+fullHash[:8]+ext)
	}
	if len(fullHash) >= 12 {
		candidates = append(candidates, stem+"_"+fullHash[:12]+ext)
	}

	for _, name := range candidates {
		candAbs := filepath.Join(destDirAbs, name)
		fi, err := os.Stat(candAbs)
		if os.IsNotExist(err) {
			return name, nil // tên còn trống
		}
		if err != nil {
			return "", fmt.Errorf("kiểm tra file đích: %w", err)
		}
		same, err := sameContent(candAbs, fi.Size(), fullHash)
		if err != nil {
			return "", err
		}
		if same {
			return name, nil // nội dung trùng khớp → tái dùng (idempotent)
		}
		// Khác nội dung → thử ứng viên tiếp theo.
	}
	return "", fmt.Errorf("không tìm được tên an toàn cho %q", baseName)
}

// sameContent cho biết file tại abs có nội dung giống hash hay không. So size
// trước (rẻ), chỉ hash file khi size khớp.
func sameContent(abs string, existingSize int64, fullHash string) (bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return false, fmt.Errorf("mở file đích: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("hash file đích: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)) == fullHash, nil
}

// FinalizeStaged chuyển file staging vào đích cuối cùng dưới dạng
// "<destDir>/<yyyymm>/<tên an toàn>". destDir là "photos" hoặc "recyclebin".
// yyyymm suy từ mtime (ngày gốc) qua yyyymmFromTime. Tên file được chọn bởi
// collisionSafeName để không bao giờ ghi đè file khác.
func (s *Storage) FinalizeStaged(tempRel, destDir, baseName, fullHash string, mtime time.Time) (*Result, error) {
	tempAbs, err := s.safeJoin(tempRel)
	if err != nil {
		return nil, err
	}

	relDir := filepath.ToSlash(filepath.Join(destDir, yyyymmFromTime(mtime)))
	destDirAbs, err := s.safeJoin(relDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destDirAbs, 0o755); err != nil {
		return nil, fmt.Errorf("tạo thư mục đích: %w", err)
	}

	name, err := s.collisionSafeName(destDirAbs, baseName, fullHash)
	if err != nil {
		return nil, err
	}

	relPath := filepath.ToSlash(filepath.Join(relDir, name))
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return nil, err
	}

	// Nếu tái dùng tên có nội dung trùng (idempotent), file đích đã tồn tại và
	// staging dư thừa → xoá staging, không rename đè.
	if _, statErr := os.Stat(abs); statErr == nil {
		_ = os.Remove(tempAbs)
	} else if err := os.Rename(tempAbs, abs); err != nil {
		_ = os.Remove(tempAbs)
		return nil, fmt.Errorf("rename vào đích: %w", err)
	}

	if !mtime.IsZero() {
		_ = os.Chtimes(abs, mtime, mtime)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat file đích: %w", err)
	}

	return &Result{
		RelPath: filepath.ToSlash(relPathFromRoot(s.root, abs)),
		AbsPath: abs,
		Size:    fi.Size(),
		SHA256:  fullHash,
	}, nil
}

// MoveCollisionSafe di chuyển file srcRel sang "<destDir>/<yyyymm>/<tên an toàn>"
// mà không ghi đè file khác. yyyymm được giữ nguyên từ segment ngày trong srcRel
// (ví dụ "recyclebin/202303/a.jpg" → giữ "202303") để bảo toàn ngày gốc; nếu
// srcRel không có segment yyyymm hợp lệ thì suy từ mtime. Dùng để khôi phục file
// từ thùng rác (recyclebin/ → photos/). File được rename trực tiếp (cùng
// filesystem), chỉ hash lại khi cần chọn tên tránh trùng.
func (s *Storage) MoveCollisionSafe(srcRel, destDir string, mtime time.Time) (*Result, error) {
	srcAbs, err := s.safeJoin(srcRel)
	if err != nil {
		return nil, err
	}

	// Giữ segment yyyymm từ srcRel nếu hợp lệ, nếu không suy từ mtime.
	yyyymm := yyyymmFromRel(srcRel)
	if yyyymm == "" {
		yyyymm = yyyymmFromTime(mtime)
	}

	baseName := filepath.Base(srcRel)

	// Hash file nguồn để collisionSafeName có thể so sánh nội dung khi trùng tên.
	fullHash, _, err := hashFileAt(srcAbs)
	if err != nil {
		return nil, err
	}

	relDir := filepath.ToSlash(filepath.Join(destDir, yyyymm))
	destDirAbs, err := s.safeJoin(relDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destDirAbs, 0o755); err != nil {
		return nil, fmt.Errorf("tạo thư mục đích: %w", err)
	}

	name, err := s.collisionSafeName(destDirAbs, baseName, fullHash)
	if err != nil {
		return nil, err
	}

	relPath := filepath.ToSlash(filepath.Join(relDir, name))
	abs, err := s.safeJoin(relPath)
	if err != nil {
		return nil, err
	}

	// Nếu đích đã tồn tại với nội dung trùng (idempotent) → xoá nguồn, không rename đè.
	if _, statErr := os.Stat(abs); statErr == nil {
		_ = os.Remove(srcAbs)
	} else if err := os.Rename(srcAbs, abs); err != nil {
		return nil, fmt.Errorf("rename vào đích: %w", err)
	}

	if !mtime.IsZero() {
		_ = os.Chtimes(abs, mtime, mtime)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat file đích: %w", err)
	}

	return &Result{
		RelPath: filepath.ToSlash(relPathFromRoot(s.root, abs)),
		AbsPath: abs,
		Size:    fi.Size(),
		SHA256:  fullHash,
	}, nil
}

// yyyymmFromRel trích segment "yyyymm" (6 chữ số) ở vị trí thứ hai của rel, ví
// dụ "recyclebin/202303/a.jpg" → "202303". Trả rỗng nếu không khớp.
func yyyymmFromRel(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 {
		seg := parts[1]
		if len(seg) == 6 {
			allDigit := true
			for _, c := range seg {
				if c < '0' || c > '9' {
					allDigit = false
					break
				}
			}
			if allDigit {
				return seg
			}
		}
	}
	return ""
}

// hashFileAt tính SHA-256 (hex) và kích thước của một file theo đường dẫn tuyệt đối.
func hashFileAt(abs string) (string, int64, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", 0, fmt.Errorf("mở file nguồn: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash file nguồn: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
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
