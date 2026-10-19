package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis bọc client go-redis với prefix key riêng để không đụng dữ liệu Immich.
type Redis struct {
	client *redis.Client
	prefix string
}

// NewRedis mở kết nối Redis. Addr rỗng → trả store no-op (M0–M1 không cần).
func NewRedis(ctx context.Context, addr, password string, db int, prefix string) (*Redis, error) {
	if addr == "" {
		return &Redis{prefix: prefix}, nil
	}
	c := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	if err := c.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Redis{client: c, prefix: prefix}, nil
}

func (r *Redis) enabled() bool { return r != nil && r.client != nil }

func (r *Redis) key(parts ...string) string {
	k := r.prefix
	for i, p := range parts {
		if i > 0 {
			k += ":"
		}
		k += p
	}
	return k
}

// Close đóng client.
func (r *Redis) Close() error {
	if r.enabled() {
		return r.client.Close()
	}
	return nil
}

var errNoRedis = errors.New("redis chưa cấu hình")

// ---- Login Flow v2: poll token ----

// PollState là trạng thái một phiên login flow đang chờ.
type PollState struct {
	LoginToken string // token nhúng trong URL /login/v2/flow/{token}
	Granted    bool   // user đã bấm "Grant access" chưa
	AppPass    string // app password sinh ra khi granted
}

// SavePoll lưu trạng thái poll theo pollToken với TTL (spec Nextcloud: 20 phút).
func (r *Redis) SavePoll(ctx context.Context, pollToken string, st PollState, ttl time.Duration) error {
	if !r.enabled() {
		return errNoRedis
	}
	key := r.key("poll", pollToken)
	fields := map[string]any{
		"login_token": st.LoginToken,
		"granted":     boolToStr(st.Granted),
		"app_pass":    st.AppPass,
	}
	if err := r.client.HSet(ctx, key, fields).Err(); err != nil {
		return err
	}
	return r.client.Expire(ctx, key, ttl).Err()
}

// GetPoll đọc trạng thái poll. ok=false nếu token không tồn tại/hết hạn.
func (r *Redis) GetPoll(ctx context.Context, pollToken string) (PollState, bool, error) {
	if !r.enabled() {
		return PollState{}, false, errNoRedis
	}
	key := r.key("poll", pollToken)
	m, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return PollState{}, false, err
	}
	if len(m) == 0 {
		return PollState{}, false, nil
	}
	return PollState{
		LoginToken: m["login_token"],
		Granted:    m["granted"] == "1",
		AppPass:    m["app_pass"],
	}, true, nil
}

// FindPollByLoginToken tìm pollToken tương ứng với một loginToken (dùng ở trang grant).
// Quét theo prefix — chấp nhận được vì LAN 1-user, số phiên rất ít.
func (r *Redis) FindPollByLoginToken(ctx context.Context, loginToken string) (string, PollState, bool, error) {
	if !r.enabled() {
		return "", PollState{}, false, errNoRedis
	}
	iter := r.client.Scan(ctx, 0, r.key("poll", "*"), 100).Iterator()
	for iter.Next(ctx) {
		full := iter.Val()
		m, err := r.client.HGetAll(ctx, full).Result()
		if err != nil {
			return "", PollState{}, false, err
		}
		if m["login_token"] == loginToken {
			pollToken := full[len(r.key("poll", "")):]
			return pollToken, PollState{
				LoginToken: m["login_token"],
				Granted:    m["granted"] == "1",
				AppPass:    m["app_pass"],
			}, true, nil
		}
	}
	if err := iter.Err(); err != nil {
		return "", PollState{}, false, err
	}
	return "", PollState{}, false, nil
}

// DeletePoll xoá một phiên poll sau khi đã trả credentials (spec: 200 chỉ một lần).
func (r *Redis) DeletePoll(ctx context.Context, pollToken string) error {
	if !r.enabled() {
		return errNoRedis
	}
	return r.client.Del(ctx, r.key("poll", pollToken)).Err()
}

// ---- App password ----

// SaveAppPassword lưu app password hợp lệ (không TTL — sống tới khi thu hồi).
func (r *Redis) SaveAppPassword(ctx context.Context, appPass, loginName string) error {
	if !r.enabled() {
		return errNoRedis
	}
	return r.client.Set(ctx, r.key("apppass", appPass), loginName, 0).Err()
}

// CheckAppPassword trả loginName nếu app password hợp lệ, ok=false nếu không.
func (r *Redis) CheckAppPassword(ctx context.Context, appPass string) (string, bool, error) {
	if !r.enabled() {
		return "", false, errNoRedis
	}
	v, err := r.client.Get(ctx, r.key("apppass", appPass)).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
