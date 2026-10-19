// Package auth cung cấp sinh token ngẫu nhiên và xác thực WebDAV.
package auth

import (
	"crypto/rand"
	"encoding/base64"
)

// NewToken sinh một token ngẫu nhiên an toàn, URL-safe, độ dài ~ n byte entropy.
func NewToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand.Read gần như không bao giờ lỗi; nếu lỗi là sự cố nghiêm trọng.
		panic("không đọc được crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
