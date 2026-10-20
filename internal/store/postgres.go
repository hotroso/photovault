// Package store quản lý metadata (PostgreSQL) và token/cache (Redis).
//
// Nguyên tắc cốt lõi (xem DESIGN.md §6b): FILE TRÊN ĐĨA là source of truth.
// Postgres chỉ là chỉ mục tra cứu; mất Postgres vẫn khôi phục được bằng reindex.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Photo là một bản ghi chỉ mục ảnh/video đã backup.
type Photo struct {
	ID           int64
	RelPath      string // đường dẫn tương đối trong DataDir, ví dụ "me/Photos/IMG_0001.HEIC"
	OriginalName string
	Size         int64
	SHA256       string
	TakenAt      time.Time // từ X-OC-Mtime nếu có, nếu không là thời điểm upload
	MIME         string
	UploadedAt   time.Time
	IsTrash      bool   // true nếu nằm trong thùng rác (recyclebin/) do trùng file
	DupOf        *int64 // ID bản gốc mà file này trùng (nil nếu là bản gốc)
	LogicalPath  string // M2: đường dẫn app nhìn thấy, ví dụ "me/Photos/IMG_0001.jpg"

	// Kích thước hiển thị (đã xoay theo EXIF) cho bố cục gallery. 0 = chưa đo,
	// -1 = không đo được (video/HEIC/ảnh lỗi). CHỈ ListPage nạp hai trường này.
	Width  int
	Height int
}

// Postgres bọc connection pool và các thao tác chỉ mục.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres mở pool kết nối từ DSN và đảm bảo schema tồn tại.
// DSN rỗng là hợp lệ cho M0–M1 (trả về store nil-an-toàn, mọi thao tác no-op).
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	if dsn == "" {
		return &Postgres{}, nil
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("mở pool postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	pg := &Postgres{pool: pool}
	if err := pg.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return pg, nil
}

// enabled cho biết store có kết nối thật hay không.
func (p *Postgres) enabled() bool { return p != nil && p.pool != nil }

// Close đóng pool.
func (p *Postgres) Close() {
	if p.enabled() {
		p.pool.Close()
	}
}

func (p *Postgres) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS photos (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rel_path      TEXT NOT NULL UNIQUE,
    original_name TEXT NOT NULL,
    size          BIGINT NOT NULL,
    sha256        TEXT NOT NULL,
    taken_at      TIMESTAMPTZ NOT NULL,
    mime          TEXT NOT NULL,
    uploaded_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS photos_taken_at_idx ON photos (taken_at DESC);
-- yyyymm + dedup toàn cục: cột thùng rác, con trỏ tới bản gốc, và chỉ mục
-- sha256 KHÔNG unique (bản gốc và bản trùng dùng chung hash).
ALTER TABLE photos ADD COLUMN IF NOT EXISTS is_trash BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE photos ADD COLUMN IF NOT EXISTS dup_of BIGINT;
CREATE INDEX IF NOT EXISTS photos_sha256_idx ON photos (sha256);
-- M2: đường dẫn logic app nhìn thấy (vd "me/Photos/x.jpg"), tách khỏi đường dẫn
-- vật lý rel_path ("photos/<yyyymm>/x.jpg"). Dùng để PROPFIND liệt kê lại cây
-- thư mục mà app đã upload. Index phục vụ tra cứu theo tiền tố thư mục.
ALTER TABLE photos ADD COLUMN IF NOT EXISTS logical_path TEXT;
CREATE INDEX IF NOT EXISTS photos_logical_path_idx ON photos (logical_path);
-- Gallery: kích thước ảnh (đo lười lúc xem, lưu lại để không đo lần hai) và
-- index cho phân trang keyset (taken_at, id) trên ảnh không-rác.
ALTER TABLE photos ADD COLUMN IF NOT EXISTS width INT NOT NULL DEFAULT 0;
ALTER TABLE photos ADD COLUMN IF NOT EXISTS height INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS photos_active_taken_idx ON photos (taken_at DESC, id DESC) WHERE is_trash = false;
`
	_, err := p.pool.Exec(ctx, schema)
	return err
}

// UpsertPhoto ghi hoặc cập nhật một bản ghi ảnh theo rel_path.
func (p *Postgres) UpsertPhoto(ctx context.Context, ph Photo) error {
	if !p.enabled() {
		return nil
	}
	const q = `
INSERT INTO photos (rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, logical_path, uploaded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (rel_path) DO UPDATE SET
    original_name = EXCLUDED.original_name,
    size          = EXCLUDED.size,
    sha256        = EXCLUDED.sha256,
    taken_at      = EXCLUDED.taken_at,
    mime          = EXCLUDED.mime,
    is_trash      = EXCLUDED.is_trash,
    dup_of        = EXCLUDED.dup_of,
    logical_path  = EXCLUDED.logical_path,
    uploaded_at   = now();
`
	_, err := p.pool.Exec(ctx, q, ph.RelPath, ph.OriginalName, ph.Size, ph.SHA256, ph.TakenAt, ph.MIME, ph.IsTrash, ph.DupOf, ph.LogicalPath)
	return err
}

// GetByRelPath trả bản ghi theo đường dẫn tương đối (dùng để chống trùng).
func (p *Postgres) GetByRelPath(ctx context.Context, relPath string) (*Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos WHERE rel_path = $1;
`
	var ph Photo
	err := p.pool.QueryRow(ctx, q, relPath).Scan(
		&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ph, nil
}

// GetByLogicalPath trả bản ghi (không thùng rác) theo đường dẫn logic app dùng,
// ví dụ "me/Photos/IMG_0001.jpg". Dùng cho GET/DELETE/MOVE và PROPFIND trên file.
func (p *Postgres) GetByLogicalPath(ctx context.Context, logicalPath string) (*Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos WHERE logical_path = $1 AND is_trash = false
ORDER BY uploaded_at DESC LIMIT 1;
`
	var ph Photo
	err := p.pool.QueryRow(ctx, q, logicalPath).Scan(
		&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ph, nil
}

// ListByLogicalPrefix trả các ảnh (không thùng rác) có logical_path bắt đầu bằng
// prefix (đã kèm dấu "/" cuối do caller). Dùng để PROPFIND liệt kê con của một
// thư mục logic. Trả tối đa limit bản ghi, mới nhất trước.
func (p *Postgres) ListByLogicalPrefix(ctx context.Context, prefix string, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos
WHERE is_trash = false AND logical_path LIKE $1
ORDER BY taken_at DESC
LIMIT $2;
`
	// Escape ký tự đặc biệt của LIKE trong prefix để tránh khớp sai.
	esc := escapeLike(prefix)
	rows, err := p.pool.Query(ctx, q, esc+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var ph Photo
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// UpdateLogicalPath đổi logical_path (và original_name) của một bản ghi theo
// rel_path vật lý. Dùng cho MOVE: file vật lý ở nguyên chỗ (layout yyyymm độc
// lập với tên logic), chỉ cập nhật ánh xạ logic.
func (p *Postgres) UpdateLogicalPath(ctx context.Context, relPath, newLogicalPath, newOriginalName string) error {
	if !p.enabled() {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		`UPDATE photos SET logical_path = $2, original_name = $3 WHERE rel_path = $1;`,
		relPath, newLogicalPath, newOriginalName,
	)
	return err
}

// escapeLike thoát các ký tự đặc biệt của toán tử LIKE (\, %, _) để prefix được
// so khớp theo nghĩa đen.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// ListByDate trả các ảnh có taken_at trong [from, to), mới nhất trước.
func (p *Postgres) ListByDate(ctx context.Context, from, to time.Time, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos
WHERE taken_at >= $1 AND taken_at < $2 AND is_trash = false
ORDER BY taken_at DESC
LIMIT $3;
`
	rows, err := p.pool.Query(ctx, q, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var ph Photo
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// ListAll trả toàn bộ ảnh (không gồm thùng rác), mới nhất trước.
func (p *Postgres) ListAll(ctx context.Context, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos WHERE is_trash = false ORDER BY taken_at DESC LIMIT $1;
`
	rows, err := p.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var ph Photo
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// GetBySHA256 trả bản gốc (tạo sớm nhất) có cùng hash — dùng để phát hiện trùng
// file toàn cục. Không có → nil, nil.
func (p *Postgres) GetBySHA256(ctx context.Context, hash string) (*Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos WHERE sha256 = $1 ORDER BY uploaded_at ASC LIMIT 1;
`
	var ph Photo
	err := p.pool.QueryRow(ctx, q, hash).Scan(
		&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ph, nil
}

// ListTrash trả các bản ghi trong thùng rác, mới nhất trước.
func (p *Postgres) ListTrash(ctx context.Context, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos WHERE is_trash = true ORDER BY taken_at DESC LIMIT $1;
`
	rows, err := p.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var ph Photo
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// GetByID trả bản ghi theo ID (dùng để tra tên bản gốc từ dup_of trong trang rác).
func (p *Postgres) GetByID(ctx context.Context, id int64) (*Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at
FROM photos WHERE id = $1;
`
	var ph Photo
	err := p.pool.QueryRow(ctx, q, id).Scan(
		&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ph, nil
}

// DeletePhotoByID xoá một bản ghi theo ID (xoá vĩnh viễn khỏi chỉ mục).
func (p *Postgres) DeletePhotoByID(ctx context.Context, id int64) error {
	if !p.enabled() {
		return nil
	}
	_, err := p.pool.Exec(ctx, `DELETE FROM photos WHERE id = $1;`, id)
	return err
}

// DeletePhotoByRelPath xoá một bản ghi theo rel_path.
func (p *Postgres) DeletePhotoByRelPath(ctx context.Context, rel string) error {
	if !p.enabled() {
		return nil
	}
	_, err := p.pool.Exec(ctx, `DELETE FROM photos WHERE rel_path = $1;`, rel)
	return err
}

// SetTrashState cập nhật rel_path, is_trash và dup_of cho bản ghi đang khôi phục
// (hoặc đưa vào thùng rác). oldRelPath định vị bản ghi, newRelPath là vị trí mới
// trên đĩa sau khi di chuyển.
func (p *Postgres) SetTrashState(ctx context.Context, oldRelPath, newRelPath string, isTrash bool, dupOf *int64) error {
	if !p.enabled() {
		return nil
	}
	_, err := p.pool.Exec(ctx,
		`UPDATE photos SET rel_path = $2, is_trash = $3, dup_of = $4 WHERE rel_path = $1;`,
		oldRelPath, newRelPath, isTrash, dupOf,
	)
	return err
}

// DeleteNotIn xoá các bản ghi mà rel_path không nằm trong tập keep (dùng cho reindex).
func (p *Postgres) DeleteNotIn(ctx context.Context, keep map[string]struct{}) (int64, error) {
	if !p.enabled() {
		return 0, nil
	}
	rows, err := p.pool.Query(ctx, `SELECT rel_path FROM photos;`)
	if err != nil {
		return 0, err
	}
	var toDelete []string
	for rows.Next() {
		var rp string
		if err := rows.Scan(&rp); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := keep[rp]; !ok {
			toDelete = append(toDelete, rp)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var n int64
	for _, rp := range toDelete {
		ct, err := p.pool.Exec(ctx, `DELETE FROM photos WHERE rel_path = $1;`, rp)
		if err != nil {
			return n, err
		}
		n += ct.RowsAffected()
	}
	return n, nil
}

// PageCursor là vị trí phân trang keyset: lấy các ảnh "cũ hơn" (taken_at, id).
type PageCursor struct {
	TakenAt time.Time
	ID      int64
}

// ListPage trả một trang ảnh không-rác, mới nhất trước, dùng phân trang keyset
// (không OFFSET) nên trang thứ 200 vẫn nhanh như trang đầu. after=nil → trang
// đầu. from/to (nil = không lọc) giới hạn taken_at trong [from, to).
func (p *Postgres) ListPage(ctx context.Context, after *PageCursor, from, to *time.Time, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	var afterTaken *time.Time
	var afterID int64
	if after != nil {
		afterTaken, afterID = &after.TakenAt, after.ID
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, is_trash, dup_of, COALESCE(logical_path,'') AS logical_path, uploaded_at, width, height
FROM photos
WHERE is_trash = false
  AND ($1::timestamptz IS NULL OR (taken_at, id) < ($1::timestamptz, $2::bigint))
  AND ($3::timestamptz IS NULL OR taken_at >= $3::timestamptz)
  AND ($4::timestamptz IS NULL OR taken_at < $4::timestamptz)
ORDER BY taken_at DESC, id DESC
LIMIT $5;
`
	rows, err := p.pool.Query(ctx, q, afterTaken, afterID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var ph Photo
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.IsTrash, &ph.DupOf, &ph.LogicalPath, &ph.UploadedAt, &ph.Width, &ph.Height); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// SetDimensions lưu kích thước hiển thị đã đo của một ảnh (-1,-1 = không đo được).
func (p *Postgres) SetDimensions(ctx context.Context, id int64, width, height int) error {
	if !p.enabled() {
		return nil
	}
	_, err := p.pool.Exec(ctx, `UPDATE photos SET width = $2, height = $3 WHERE id = $1;`, id, width, height)
	return err
}

// CountByTrash đếm số bản ghi trong thùng rác (trash=true) hoặc ảnh thường (false).
func (p *Postgres) CountByTrash(ctx context.Context, trash bool) (int64, error) {
	if !p.enabled() {
		return 0, nil
	}
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM photos WHERE is_trash = $1;`, trash).Scan(&n)
	return n, err
}

// Count trả tổng số ảnh đã index.
func (p *Postgres) Count(ctx context.Context) (int64, error) {
	if !p.enabled() {
		return 0, nil
	}
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM photos;`).Scan(&n)
	return n, err
}
