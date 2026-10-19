package compat

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/local/photovault/internal/auth"
	"github.com/local/photovault/internal/config"
	"github.com/local/photovault/internal/httpx"
	"github.com/local/photovault/internal/store"
)

// pollTTL: spec Nextcloud cho token sống 20 phút.
const pollTTL = 20 * time.Minute

// LoginFlow gom các handler của Login Flow v2.
type LoginFlow struct {
	cfg    *config.Config
	redis  *store.Redis
	logger *slog.Logger
}

// NewLoginFlow tạo bộ handler login flow.
func NewLoginFlow(cfg *config.Config, redis *store.Redis, logger *slog.Logger) *LoginFlow {
	return &LoginFlow{cfg: cfg, redis: redis, logger: logger}
}

// initResp khớp đúng cấu trúc init của Nextcloud thật:
// { "poll": { "token", "endpoint" }, "login" }.
type initResp struct {
	Poll  pollObj `json:"poll"`
	Login string  `json:"login"`
}

type pollObj struct {
	Token    string `json:"token"`
	Endpoint string `json:"endpoint"`
}

// Init xử lý POST /index.php/login/v2 (và /login/v2).
// Sinh poll token + login token, lưu Redis, trả URL tuyệt đối.
func (lf *LoginFlow) Init(w http.ResponseWriter, r *http.Request) {
	pollToken := auth.NewToken(48)
	loginToken := auth.NewToken(48)

	st := store.PollState{LoginToken: loginToken, Granted: false}
	if err := lf.redis.SavePoll(r.Context(), pollToken, st, pollTTL); err != nil {
		lf.logger.Error("lưu poll state thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	httpx.JSON(w, lf.logger, http.StatusOK, initResp{
		Poll: pollObj{
			Token:    pollToken,
			Endpoint: lf.cfg.PublicURL + "/login/v2/poll",
		},
		Login: lf.cfg.PublicURL + "/login/v2/flow/" + loginToken,
	})
}

// credsResp khớp LoginFlowV2Credentials::jsonSerialize() của Nextcloud thật.
type credsResp struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

// Poll xử lý POST /login/v2/poll với body form "token".
//   - chưa granted → 404 với body [] (mảng rỗng) đúng như spec.
//   - granted      → 200 với { server, loginName, appPassword }, CHỈ MỘT LẦN.
func (lf *LoginFlow) Poll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// App Nextcloud iOS gửi token qua query string ở endpoint poll (không phải
	// body form). r.Form gộp cả query lẫn body nên bắt được token dù nằm ở đâu;
	// r.PostForm chỉ có body → sẽ rỗng và luôn trả 404 (bug cũ).
	token := r.Form.Get("token")

	st, ok, err := lf.redis.GetPoll(r.Context(), token)
	if err != nil {
		lf.logger.Error("đọc poll state thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok || !st.Granted {
		// Chưa xong: 404 với body "[]" (mảng rỗng) — đúng hành vi Nextcloud thật.
		httpx.JSON(w, lf.logger, http.StatusNotFound, []any{})
		return
	}

	// Granted: trả credentials và xoá phiên (200 chỉ một lần).
	resp := credsResp{
		Server:      lf.cfg.PublicURL,
		LoginName:   lf.cfg.User,
		AppPassword: st.AppPass,
	}
	if err := lf.redis.DeletePoll(r.Context(), token); err != nil {
		lf.logger.Warn("xoá poll state thất bại", "err", err)
	}
	httpx.JSON(w, lf.logger, http.StatusOK, resp)
}

// grantPage là trang "Grant access" đơn giản thay cho cả chuỗi auth-picker→grant
// của Nextcloud thật. LAN 1-user nên không cần nhập mật khẩu: bấm nút là cấp.
var grantPage = `<!doctype html><html lang="vi"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Cấp quyền truy cập</title>
<style>
 body{font-family:system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f6f7f9}
 .box{max-width:360px;padding:2rem;background:#fff;border-radius:16px;box-shadow:0 2px 16px rgba(0,0,0,.08);text-align:center}
 button{font-size:1rem;padding:.75rem 1.5rem;border:0;border-radius:10px;background:#0a7ea4;color:#fff;cursor:pointer}
 .done{color:#157f3b;font-weight:600}
</style></head><body><div class="box">
 <h2>Backup ảnh iPhone</h2>
 <p>Ứng dụng muốn kết nối để backup ảnh. Bấm để cấp quyền.</p>
 <form method="POST" action="/login/v2/grant">
   <input type="hidden" name="login_token" value="{{TOKEN}}">
   <button type="submit">Cấp quyền truy cập</button>
 </form>
</div></body></html>`

var grantDonePage = `<!doctype html><html lang="vi"><head><meta charset="utf-8">
<title>Xong</title><style>body{font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:100vh}</style></head>
<body><div><h2 style="color:#157f3b">✓ Đã cấp quyền</h2><p>Bạn có thể quay lại ứng dụng.</p></div></body></html>`

// Landing xử lý GET /login/v2/flow/{token}: hiển thị trang Grant.
func (lf *LoginFlow) Landing(w http.ResponseWriter, r *http.Request) {
	loginToken := r.PathValue("token")
	if loginToken == "" {
		http.Error(w, "thiếu token", http.StatusBadRequest)
		return
	}
	// Xác nhận loginToken tồn tại (có phiên poll tương ứng).
	_, _, ok, err := lf.redis.FindPollByLoginToken(r.Context(), loginToken)
	if err != nil {
		lf.logger.Error("tìm poll theo login token thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "token không hợp lệ hoặc đã hết hạn", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(replaceToken(grantPage, loginToken)))
}

// Grant xử lý POST /login/v2/grant: sinh app password, đánh dấu phiên granted.
func (lf *LoginFlow) Grant(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	loginToken := r.PostForm.Get("login_token")
	pollToken, st, ok, err := lf.redis.FindPollByLoginToken(r.Context(), loginToken)
	if err != nil {
		lf.logger.Error("tìm poll theo login token thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "token không hợp lệ hoặc đã hết hạn", http.StatusForbidden)
		return
	}

	appPass := auth.NewToken(48)
	st.Granted = true
	st.AppPass = appPass
	if err := lf.redis.SavePoll(r.Context(), pollToken, st, pollTTL); err != nil {
		lf.logger.Error("cập nhật poll state thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := lf.redis.SaveAppPassword(r.Context(), appPass, lf.cfg.User); err != nil {
		lf.logger.Error("lưu app password thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(grantDonePage))
}

// GetAppPassword xử lý GET /ocs/v2.php/core/getapppassword (Basic Auth) — tiện test.
// Ở đây mọi Basic Auth với đúng username đều được cấp (LAN 1-user, không có mật
// khẩu thật). Trả XML OCS giống Nextcloud.
func (lf *LoginFlow) GetAppPassword(w http.ResponseWriter, r *http.Request) {
	user, _, ok := r.BasicAuth()
	if !ok || user != lf.cfg.User {
		w.Header().Set("WWW-Authenticate", `Basic realm="Nextcloud"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	appPass := auth.NewToken(48)
	if err := lf.redis.SaveAppPassword(r.Context(), appPass, lf.cfg.User); err != nil {
		lf.logger.Error("lưu app password thất bại", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?>
<ocs><meta><status>ok</status><statuscode>200</statuscode><message>OK</message></meta>` +
		`<data><apppassword>` + appPass + `</apppassword></data></ocs>`))
}

// replaceToken thay {{TOKEN}} trong template đơn giản.
func replaceToken(tpl, token string) string {
	out := make([]byte, 0, len(tpl))
	needle := "{{TOKEN}}"
	for {
		i := indexOf(tpl, needle)
		if i < 0 {
			out = append(out, tpl...)
			break
		}
		out = append(out, tpl[:i]...)
		out = append(out, token...)
		tpl = tpl[i+len(needle):]
	}
	return string(out)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
