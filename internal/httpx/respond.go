package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// JSON ghi một giá trị bất kỳ thành JSON với status code cho trước.
func JSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Error("ghi JSON thất bại", "err", err)
	}
}

// OCSMeta là khối "meta" chuẩn trong mọi response OCS của Nextcloud.
type OCSMeta struct {
	Status     string `json:"status"`
	StatusCode int    `json:"statuscode"`
	Message    string `json:"message"`
}

// OCSResponse bọc dữ liệu theo đúng hình dạng { "ocs": { "meta": ..., "data": ... } }
// mà client Nextcloud mong đợi từ các endpoint /ocs/....
type OCSResponse struct {
	OCS ocsBody `json:"ocs"`
}

type ocsBody struct {
	Meta OCSMeta `json:"meta"`
	Data any     `json:"data"`
}

// OCS ghi một response OCS thành công (status "ok", statuscode 200) với data cho trước.
func OCS(w http.ResponseWriter, logger *slog.Logger, data any) {
	JSON(w, logger, http.StatusOK, OCSResponse{
		OCS: ocsBody{
			Meta: OCSMeta{Status: "ok", StatusCode: 200, Message: "OK"},
			Data: data,
		},
	})
}
