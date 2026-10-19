// Package config nạp cấu hình từ file .env (và biến môi trường) cho server.
//
// M0–M1 cố tình chỉ dùng thư viện chuẩn: .env được parse thủ công để tránh phụ
// thuộc ngoài, giúp build chạy ngay. Các kết nối PostgreSQL/Redis chỉ được đọc
// vào struct ở đây; việc thực sự mở kết nối sẽ thêm ở M2/M3.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config chứa toàn bộ tham số runtime của server.
type Config struct {
	ListenAddr  string // địa chỉ lắng nghe, ví dụ 0.0.0.0:8081
	PublicURL   string // URL công khai iPhone thấy, ví dụ https://192.168.1.55:8081
	User        string // username LAN 1-user
	DisplayName string // tên hiển thị
	DataDir     string // thư mục lưu ảnh raw

	TLSCert string // đường dẫn cert (rỗng = không tự chạy TLS)
	TLSKey  string // đường dẫn key

	PGDSN string // DSN PostgreSQL (DB riêng, không đụng Immich)

	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisPrefix   string

	NCVersion       string // ví dụ 28.0.0.0
	NCVersionString string // ví dụ 28.0.0

	LogLevel string // debug|info|warn|error
}

// Load đọc file .env tại envPath (nếu tồn tại) rồi phủ bằng biến môi trường thật,
// sau đó map vào Config kèm giá trị mặc định hợp lý.
func Load(envPath string) (*Config, error) {
	if err := loadDotenv(envPath); err != nil {
		return nil, err
	}

	redisDB, err := strconv.Atoi(getenv("APP_REDIS_DB", "15"))
	if err != nil {
		return nil, fmt.Errorf("APP_REDIS_DB không phải số nguyên: %w", err)
	}

	cfg := &Config{
		ListenAddr:      getenv("APP_LISTEN_ADDR", "0.0.0.0:8081"),
		PublicURL:       strings.TrimRight(getenv("APP_PUBLIC_URL", "https://127.0.0.1:8081"), "/"),
		User:            getenv("APP_USER", "me"),
		DisplayName:     getenv("APP_DISPLAY_NAME", "Me"),
		DataDir:         getenv("APP_DATA_DIR", "./data"),
		TLSCert:         getenv("APP_TLS_CERT", ""),
		TLSKey:          getenv("APP_TLS_KEY", ""),
		PGDSN:           getenv("APP_PG_DSN", ""),
		RedisAddr:       getenv("APP_REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:   getenv("APP_REDIS_PASSWORD", ""),
		RedisDB:         redisDB,
		RedisPrefix:     getenv("APP_REDIS_PREFIX", "photovault:"),
		NCVersion:       getenv("APP_NC_VERSION", "28.0.0.0"),
		NCVersionString: getenv("APP_NC_VERSION_STRING", "28.0.0"),
		LogLevel:        getenv("APP_LOG_LEVEL", "info"),
	}
	return cfg, nil
}

// UsesTLS cho biết server có tự chạy TLS hay không (khi được cấp cert+key).
func (c *Config) UsesTLS() bool {
	return c.TLSCert != "" && c.TLSKey != ""
}

// loadDotenv nạp các cặp KEY=VALUE từ file .env vào môi trường nếu KEY chưa được
// set sẵn (biến môi trường thật luôn thắng). File không tồn tại là hợp lệ.
func loadDotenv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("mở %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			return fmt.Errorf("%s dòng %d: thiếu dấu '='", path, lineNo)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Bỏ cặp nháy bao quanh nếu có.
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return sc.Err()
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
