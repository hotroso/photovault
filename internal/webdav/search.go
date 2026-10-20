package webdav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WebDAV SEARCH (RFC 5323 basicsearch) — tab Media của app Nextcloud iOS gửi:
//
//	SEARCH /remote.php/dav
//	  scope:  /files/<user>/<thư mục media>, depth infinity
//	  where:  getcontenttype LIKE image/% OR video/%
//	          AND getlastmodified <= <lessDate> AND >= <greaterDate>  (RFC 3339)
//	  limit:  nresults
//	  header: X-NC-Paginate / -Offset / -Count khi app phân trang
//
// Server chỉ hỗ trợ đúng dạng truy vấn media này. Truy vấn khác (vd tìm theo
// tên) trả danh sách rỗng thay vì lỗi.

const (
	searchMaxResults   = 50000 // trần số kết quả một truy vấn
	searchDefaultCount = 200   // cỡ trang khi app phân trang mà không gửi Count
)

type searchQuery struct {
	scope string     // "/files/me/Photos"
	from  *time.Time // gte/gt
	to    *time.Time // lte/lt
	limit int
	media bool // có điều kiện getcontenttype image/% hoặc video/%
}

// ServeSearch xử lý SEARCH /remote.php/dav.
func (h *Handler) ServeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != "SEARCH" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "đọc body lỗi", http.StatusBadRequest)
		return
	}
	q, err := parseSearch(body)
	if err != nil {
		http.Error(w, "truy vấn SEARCH không hợp lệ", http.StatusBadRequest)
		return
	}

	ms := newMultistatus()
	user := h.cfg.User
	scope := strings.Trim(strings.TrimPrefix(strings.Trim(q.scope, "/"), "files/"), "/")
	if scope == "" {
		scope = user
	}
	if !q.media || (scope != user && !strings.HasPrefix(scope, user+"/")) {
		writeMultistatus(w, h.logger, ms)
		return
	}

	limit := q.limit
	if limit <= 0 || limit > searchMaxResults {
		limit = searchMaxResults
	}
	photos, err := h.pg.SearchMedia(r.Context(), scope+"/", q.from, q.to, limit)
	if err != nil {
		h.logger.Error("SEARCH truy vấn lỗi", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Phân trang kiểu Nextcloud: app gửi offset/count, server báo còn trang sau
	// bằng X-NC-Paginate: true.
	page := photos
	if strings.EqualFold(r.Header.Get("X-NC-Paginate"), "true") {
		offset, _ := strconv.Atoi(r.Header.Get("X-NC-Paginate-Offset"))
		count, err := strconv.Atoi(r.Header.Get("X-NC-Paginate-Count"))
		if err != nil || count <= 0 {
			count = searchDefaultCount
		}
		offset = min(max(offset, 0), len(photos))
		end := min(offset+count, len(photos))
		page = photos[offset:end]
		if end < len(photos) {
			w.Header().Set("X-NC-Paginate", "true")
			w.Header().Set("X-NC-Paginate-Token", "photovault")
			w.Header().Set("X-NC-Paginate-Total", strconv.Itoa(len(photos)))
		}
	}

	for i := range page {
		p := &page[i]
		if p.LogicalPath == "" {
			continue
		}
		ms.Responses = append(ms.Responses, h.responseForPhoto(davHref(p.LogicalPath, false), p))
	}
	writeMultistatus(w, h.logger, ms)
}

// parseSearch rút phạm vi, khoảng ngày, giới hạn và điều kiện media từ body
// basicsearch. Chỉ dựa vào tên phần tử (bỏ namespace) nên chịu được các biến
// thể nhỏ giữa các phiên bản app (lt/lte, gt/gte).
func parseSearch(body []byte) (searchQuery, error) {
	var q searchQuery
	dec := xml.NewDecoder(bytes.NewReader(body))
	var stack []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return q, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, strings.ToLower(t.Name.Local))
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			text := strings.TrimSpace(string(t))
			if text == "" || len(stack) == 0 {
				continue
			}
			switch stack[len(stack)-1] {
			case "href":
				if contains(stack, "scope") {
					q.scope = text
				}
			case "nresults":
				q.limit, _ = strconv.Atoi(text)
			case "literal":
				lower := strings.ToLower(text)
				if strings.HasPrefix(lower, "image/") || strings.HasPrefix(lower, "video/") {
					q.media = true
					continue
				}
				if len(stack) < 2 {
					continue
				}
				ts, ok := parseSearchTime(text)
				if !ok {
					continue
				}
				switch stack[len(stack)-2] {
				case "lt", "lte":
					q.to = &ts
				case "gt", "gte":
					q.from = &ts
				}
			}
		}
	}
	return q, nil
}

func contains(stack []string, name string) bool {
	for _, s := range stack {
		if s == name {
			return true
		}
	}
	return false
}

// parseSearchTime nhận RFC 3339 (app hiện tại), định dạng HTTP, hoặc unix giây.
func parseSearchTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z0700", http.TimeFormat, time.RFC1123Z} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}
