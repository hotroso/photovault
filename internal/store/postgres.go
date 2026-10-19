// Package store quản lý metadata (PostgreSQL) và token/cache (Redis).
//
// Nguyên tắc cốt lõi (xem DESIGN.md §6b): FILE TRÊN ĐĨA là source of truth.
// Postgres chỉ là chỉ mục tra cứu; mất Postgres vẫn khôi phục được bằng reindex.
package store

import (
	"context"
	"errors"
	"fmt"
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
INSERT INTO photos (rel_path, original_name, size, sha256, taken_at, mime, uploaded_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (rel_path) DO UPDATE SET
    original_name = EXCLUDED.original_name,
    size          = EXCLUDED.size,
    sha256        = EXCLUDED.sha256,
    taken_at      = EXCLUDED.taken_at,
    mime          = EXCLUDED.mime,
    uploaded_at   = now();
`
	_, err := p.pool.Exec(ctx, q, ph.RelPath, ph.OriginalName, ph.Size, ph.SHA256, ph.TakenAt, ph.MIME)
	return err
}

// GetByRelPath trả bản ghi theo đường dẫn tương đối (dùng để chống trùng).
func (p *Postgres) GetByRelPath(ctx context.Context, relPath string) (*Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, uploaded_at
FROM photos WHERE rel_path = $1;
`
	var ph Photo
	err := p.pool.QueryRow(ctx, q, relPath).Scan(
		&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.UploadedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ph, nil
}

// ListByDate trả các ảnh có taken_at trong [from, to), mới nhất trước.
func (p *Postgres) ListByDate(ctx context.Context, from, to time.Time, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, uploaded_at
FROM photos
WHERE taken_at >= $1 AND taken_at < $2
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
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
}

// ListAll trả toàn bộ ảnh, mới nhất trước (dùng cho dashboard khi không lọc).
func (p *Postgres) ListAll(ctx context.Context, limit int) ([]Photo, error) {
	if !p.enabled() {
		return nil, nil
	}
	const q = `
SELECT id, rel_path, original_name, size, sha256, taken_at, mime, uploaded_at
FROM photos ORDER BY taken_at DESC LIMIT $1;
`
	rows, err := p.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Photo
	for rows.Next() {
		var ph Photo
		if err := rows.Scan(&ph.ID, &ph.RelPath, &ph.OriginalName, &ph.Size, &ph.SHA256, &ph.TakenAt, &ph.MIME, &ph.UploadedAt); err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, rows.Err()
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

// Count trả tổng số ảnh đã index.
func (p *Postgres) Count(ctx context.Context) (int64, error) {
	if !p.enabled() {
		return 0, nil
	}
	var n int64
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM photos;`).Scan(&n)
	return n, err
}
