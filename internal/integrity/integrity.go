// Package integrity cài hai lệnh bảo vệ dữ liệu (DESIGN.md §6b):
//   - Verify: quét đĩa, so SHA-256 với Postgres, báo file lệch/thiếu.
//   - Reindex: quét đĩa dựng lại bảng Postgres (phục hồi khi mất DB).
package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/photovault/internal/store"
)

// walk liệt kê mọi file thường trong root (bỏ .uploads/, .part, file ẩn .thumbs).
func walk(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".uploads" || name == ".thumbs" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

func relOf(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyReport tổng hợp kết quả verify.
type VerifyReport struct {
	Checked     int
	OK          int
	Mismatched  []string // file có SHA-256 khác với DB (nghi hỏng/bit rot)
	MissingInDB []string // có trên đĩa, không có trong DB
	MissingOnFS []string // có trong DB, mất trên đĩa
}

// Verify quét root, so với Postgres.
func Verify(ctx context.Context, root string, pg *store.Postgres) (*VerifyReport, error) {
	files, err := walk(root)
	if err != nil {
		return nil, err
	}
	rep := &VerifyReport{}
	seen := map[string]struct{}{}

	for _, abs := range files {
		rel := relOf(root, abs)
		seen[rel] = struct{}{}
		rep.Checked++

		sum, _, err := hashFile(abs)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", rel, err)
		}
		rec, err := pg.GetByRelPath(ctx, rel)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			rep.MissingInDB = append(rep.MissingInDB, rel)
			continue
		}
		if rec.SHA256 != sum {
			rep.Mismatched = append(rep.Mismatched, rel)
			continue
		}
		rep.OK++
	}

	// File có trong DB nhưng mất trên đĩa.
	all, err := pg.ListAll(ctx, 1_000_000)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if _, ok := seen[p.RelPath]; !ok {
			rep.MissingOnFS = append(rep.MissingOnFS, p.RelPath)
		}
	}
	return rep, nil
}

// ReindexReport tổng hợp kết quả reindex.
type ReindexReport struct {
	Indexed int
	Removed int64
}

// Reindex quét root, upsert mọi file vào Postgres, xoá bản ghi không còn trên đĩa.
func Reindex(ctx context.Context, root string, pg *store.Postgres) (*ReindexReport, error) {
	files, err := walk(root)
	if err != nil {
		return nil, err
	}
	keep := map[string]struct{}{}
	rep := &ReindexReport{}

	for _, abs := range files {
		rel := relOf(root, abs)
		keep[rel] = struct{}{}

		sum, size, err := hashFile(abs)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", rel, err)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, err
		}
		taken := fi.ModTime()
		if taken.IsZero() {
			taken = time.Now()
		}
		ph := store.Photo{
			RelPath:      rel,
			OriginalName: filepath.Base(rel),
			Size:         size,
			SHA256:       sum,
			TakenAt:      taken,
			MIME:         mimeByExt(rel),
		}
		if err := pg.UpsertPhoto(ctx, ph); err != nil {
			return nil, err
		}
		rep.Indexed++
	}

	removed, err := pg.DeleteNotIn(ctx, keep)
	if err != nil {
		return nil, err
	}
	rep.Removed = removed
	return rep, nil
}

func mimeByExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".heic", ".heif":
		return "image/heic"
	case ".mov":
		return "video/quicktime"
	case ".mp4", ".m4v":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}
