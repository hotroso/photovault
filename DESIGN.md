# photovault — Server Go tương thích Nextcloud, backup RAW ảnh iPhone về máy tính

> Mục tiêu duy nhất: **CÓ MỘT BẢN COPY RAW (NGUYÊN BYTES) TOÀN BỘ ẢNH/VIDEO TỪ
> IPHONE VỀ MÁY TÍNH**, dùng app Nextcloud iOS chính thức (miễn phí) làm client,
> và một server Go **tương thích giao thức Nextcloud** thay cho server PHP nặng nề.
>
> Tinh thần giống **cắm cáp copy thư mục DCIM**: chỉ chép đúng file gốc xuống đĩa,
> không re-encode, không đổi định dạng, không đòi quyền đặc biệt. Server chỉ là
> cái "cổng" để iPhone tự đẩy file qua mạng thay vì cắm dây.

---

## 1. Bối cảnh & lý do (đọc kỹ phần này)

Bạn **đã dùng Immich** để backup, nhưng **không yên tâm** vì:
- Immich **xử lý/transcode** ảnh-video, sinh thumbnail, đổi tổ chức file → bạn sợ
  file gốc bị biến đổi hoặc hỏng, và khó lấy lại đúng byte gốc.
- Bạn muốn một bản sao **raw, bất biến, dễ kiểm chứng** — mở ra là đúng file như
  trên iPhone, không phụ thuộc app nào để đọc.

→ Giải pháp này **không thay thế Immich**; nó là **lớp an toàn thứ hai**: một bản
copy nguyên bytes nằm cạnh kho Immich, để nếu Immich có sự cố thì bạn vẫn còn file gốc.

**Hệ quả thiết kế bắt buộc:**
1. Server **ghi đúng byte nhận được**, **không** transcode/nén/sửa EXIF.
2. Giữ **tên gốc + ngày chụp** (qua `X-OC-Mtime`).
3. Lưu **checksum (SHA-256)** mỗi file để **kiểm chứng toàn vẹn** về sau (chống "sợ hỏng file").
4. Tái dùng hạ tầng sẵn có của Immich: **PostgreSQL** (metadata) + **Redis** (token/cache).

---

## 1b. Vì sao BẮT BUỘC dùng app Nextcloud (chốt 4 ràng buộc)

Đây là mục then chốt giải thích vì sao phải chọn đúng app Nextcloud và phải giả
lập handshake — để sau này không ai (kể cả chính bạn) lung lay lại quyết định.

Bốn ràng buộc cứng của dự án:

| # | Ràng buộc | Hệ quả |
|---|---|---|
| 1 | **Không có Macbook** | Không build/ký được app iOS riêng → **bắt buộc dùng ké app có sẵn trên App Store**. |
| 2 | **Phải miễn phí** | Các app upload WebDAV khác (PhotoSync, Pho, PixCarry...) **thu phí** cho phần auto-upload. |
| 3 | **Auto-upload nền** ("đặt rồi quên") | Loại các app WebDAV miễn phí kiểu trình duyệt file (upload thủ công, có quảng cáo). |
| 4 | **Raw + tự kiểm soát tổ chức + verify** | Chỉ đạt được khi **tự viết server** (Immich/Nextcloud thật không cho bạn vòng verify chủ động). |

**Kết luận:** App **Nextcloud iOS là client miễn phí DUY NHẤT** thỏa đồng thời cả
bốn (miễn phí hoàn toàn + auto-upload nền + có sẵn, không cần Macbook). Đã chọn
app Nextcloud thì **không có đường vòng nào tránh được việc giả lập handshake** —
đó là cái giá bắt buộc, không phải lựa chọn tùy ý.

→ Vì vậy hướng **Go-giả-lập-Nextcloud KHÔNG phải hạ sách**; với 4 ràng buộc trên,
nó là lựa chọn đúng và gần như duy nhất.

> **Đính chính một ngộ nhận (ràng buộc "PHP kém Go"):** việc đổi PHP → Go **không**
> tự nó làm hết hỏng file. Với tác vụ nhận-byte-ghi-đĩa, cả PHP lẫn Go đều chỉ
> `write()` xuống đĩa; ngôn ngữ **không quyết định** độ toàn vẹn. Thứ **thật sự**
> chữa nỗi lo hỏng file là **verify checksum đầu–cuối** (§6b). Lý do chính đáng để
> chọn Go ở đây là **quyền kiểm soát code + dễ nhúng vòng verify + nhẹ**, không phải
> vì PHP làm hỏng byte. Hãy chọn Go vì điều đó, đừng vì niềm tin "PHP hỏng file".

---

## 2. Đánh giá giải pháp (góc nhìn kỹ sư)

### Ý tưởng gốc
> "Cài app Nextcloud iOS, trỏ vào một WebDAV server siêu nhẹ viết bằng Go."

### Nhận định
Khả thi và đáng làm, nhưng phải sửa một hiểu lầm cốt lõi:

> **App Nextcloud iOS KHÔNG chỉ nói WebDAV thuần.** Trước khi được phép upload,
> app bắt buộc đi qua chuỗi **handshake** gồm các endpoint JSON/OCS đặc thù của
> Nextcloud. Server không trả đúng các endpoint này → app báo lỗi ngay ở màn hình
> đăng nhập và **không bao giờ gọi tới WebDAV**.

Nên "server fake" = **WebDAV server** + **lớp giả lập handshake Nextcloud**. Khối
lượng vừa phải, rất hợp Go, và vẫn nhẹ hơn Nextcloud PHP vài bậc độ lớn.

### So sánh nhanh các hướng

| Hướng | Ưu | Nhược |
|---|---|---|
| **Nextcloud PHP đầy đủ** | Chạy chắc chắn | Nặng (PHP+DB+Redis riêng), đúng thứ bạn muốn tránh |
| **Server Go giả lập (chọn)** | Nhẹ, nhanh, raw bytes, kiểm soát hoàn toàn, thêm dashboard | Phải tự giả lập handshake; phải bám sát protocol |
| **Viết app iOS riêng** | Toàn quyền | **Chặn ngay**: không có Macbook để build/ký |
| **WebDAV thuần (rsync/davfs)** | Cực đơn giản | App Nextcloud không kết nối được (thiếu handshake) |

Kết luận: **hướng Go giả lập là lựa chọn đúng** cho ràng buộc của bạn.

### Rủi ro & độ khó (xếp theo mức độ)
1. **Handshake đăng nhập** (khó "đoán cho đúng" nhất): `status.php`, `capabilities`, Login Flow v2.
2. **WebDAV upload**: `PROPFIND`/`MKCOL`/`PUT`, và **chunked upload v2** cho video lớn.
3. **HTTPS/ATS trên iOS** (§7) — đã chốt dùng HTTPS nên chủ động xử lý từ đầu.

---

## 3. Giả định đã chốt

- **Một chiều, raw**: iPhone → máy tính, ghi nguyên byte, không xử lý.
- **Metadata**: **PostgreSQL** (tái dùng instance của Immich, **tạo database/schema riêng**, không đụng schema Immich).
- **Token/cache**: **Redis** (tái dùng instance của Immich, **tách namespace/DB index riêng**).
- **Dashboard**: `/dashboard` với **thumbnail + lọc theo ngày**, tách khỏi phần API Nextcloud.
- **Auth**: Login Flow v2 (chính) + Basic Auth (test nhanh).
- **Transport**: **HTTPS** (đã chốt). Xem §7 cách lấy cert cho IP LAN + tin cậy trên iOS.
- **LAN only, 1 user** (chính bạn). Không multi-tenant thật.

---

## 4. Kiến trúc tổng thể

```
┌─────────────────┐        LAN (HTTPS :8081)       ┌────────────────────────────────┐
│  iPhone          │ ─────────────────────────────▶│  Go Fake Nextcloud Server        │
│  Nextcloud iOS   │   1. Handshake (status/caps)   │                                  │
│  (Auto Upload)   │   2. Login Flow v2 → token     │  ┌────────────────────────────┐  │
│                  │   3. WebDAV PUT/MKCOL/MOVE     │  │ Nextcloud-compat layer      │  │
└─────────────────┘   (ảnh + video chunked)        │  │  /status.php                │  │
                                                     │  │  /ocs/.../capabilities,user │  │
                                                     │  │  /index.php/login/v2        │  │
                                                     │  │  /remote.php/dav/...         │  │
                                                     │  └──────┬───────────┬─────────┘  │
                                                     │         │ raw bytes  │ token      │
                                                     │         ▼            ▼            │
                                                     │  ┌────────────┐  ┌──────────┐     │
                                                     │  │ Local disk  │  │  Redis    │     │
                                                     │  │ ./data/<usr>│  │ (Immich)  │     │
                                                     │  └─────┬──────┘  └──────────┘     │
                                                     │        │ index + checksum          │
                                                     │        ▼                           │
                                                     │  ┌──────────────┐                  │
                                                     │  │ PostgreSQL     │◀── Dashboard    │
                                                     │  │ (Immich inst., │    /dashboard   │
                                                     │  │  DB riêng)     │    thumbnail+   │
                                                     │  └──────────────┘    lọc ngày      │
                                                     └────────────────────────────────┘
```

### Thành phần
- **compat layer**: handler trả đúng định dạng Nextcloud để qua handshake + nhận WebDAV.
- **storage**: ghi **raw bytes** xuống `./data/<user>/...`; file là nguồn chân lý (source of truth).
- **postgres**: bảng chỉ mục file (path, tên gốc, kích thước, mtime, sha256, taken_at, mime). Mất DB vẫn khôi phục được bằng cách quét lại đĩa.
- **redis**: lưu poll-token Login Flow v2 (TTL 20 phút), app password, cache nhẹ.
- **dashboard**: trang web xem ảnh (thumbnail + lọc ngày), đọc từ Postgres + đĩa.

### Vì sao file-trên-đĩa là source of truth, Postgres chỉ là index
Phù hợp mục tiêu "raw & an toàn": kể cả khi Postgres/Redis hỏng, **ảnh gốc vẫn
nằm nguyên trên đĩa**. Có một lệnh `reindex` quét `./data` dựng lại bảng Postgres
từ đầu. DB là tiện ích tra cứu, **không** phải nơi giữ dữ liệu quý.

---

## 5. Các endpoint PHẢI giả lập (từ tài liệu Nextcloud chính thức)

> Nguồn: tài liệu nhà phát triển Nextcloud — [Login Flow](https://docs.nextcloud.com/server/stable/developer_manual/client_apis/LoginFlow/index.html),
> [WebDAV Basic](https://docs.nextcloud.com/server/stable/developer_manual/client_apis/WebDAV/basic.html),
> [Chunked upload](https://docs.nextcloud.com/server/stable/developer_manual/client_apis/WebDAV/chunking.html).
> *Nội dung đã được diễn giải lại cho phù hợp giấy phép.*

### 5.1 Phát hiện server
- `GET /status.php` → JSON báo "đây là Nextcloud" + version:
  ```json
  { "installed": true, "maintenance": false, "needsDbUpgrade": false,
    "version": "28.0.0.0", "versionstring": "28.0.0", "edition": "",
    "productname": "Nextcloud", "extendedSupport": false }
  ```
  Bịa `version` đủ mới để app chấp nhận.

### 5.2 Capabilities (OCS)
- `GET /ocs/v2.php/cloud/capabilities` (header `OCS-APIRequest: true`) → khai báo
  năng lực. Tối thiểu khối `core` (`webdav-root`: `remote.php/webdav`) và `files`
  (bật `bigfilechunking`) để app dùng chunked upload.
- `GET /ocs/v1.php/cloud/user` + `/ocs/v2.php/cloud/user` → trả `id`/display name.

### 5.3 Login Flow v2 (luồng đăng nhập chính)
1. `POST /index.php/login/v2` (ẩn danh) → trả:
   ```json
   { "poll": { "token": "<poll-token>", "endpoint": "https://192.168.1.55:8081/login/v2/poll" },
     "login": "https://192.168.1.55:8081/login/v2/flow/<login-token>" }
   ```
2. App mở `login` URL trong **trình duyệt mặc định** iPhone → server Go hiện trang
   "Grant access" → bạn bấm đồng ý → sinh **app password**.
3. App **poll** `POST /login/v2/poll` body `token=<poll-token>`:
   - Chưa cấp → **HTTP 404** (đúng spec; không trả 200 sớm).
   - Đã cấp → **200 đúng một lần**:
     ```json
     { "server": "https://192.168.1.55:8081", "loginName": "me",
       "appPassword": "<token-ngẫu-nhiên>" }
     ```
> Spec: 200 chỉ trả một lần; poll-token sống 20 phút (lưu Redis với TTL). `loginName`
> dùng để poll phải khớp `loginName` dùng sinh URL WebDAV. `server` phải đúng scheme
> `https://` đang chạy, nếu không app nối sai URL.

### 5.4 (Tuỳ chọn) Basic Auth / App password
- `GET /ocs/v2.php/core/getapppassword` (Basic Auth) → sinh app password; tiện test `curl`.
- Mọi WebDAV request chấp nhận `Authorization: Basic ...`. LAN 1-user → token store trong Redis/Postgres.

### 5.5 WebDAV — nơi ảnh thật sự được đẩy lên
Base `/remote.php/dav`:
- `PROPFIND /remote.php/dav/files/<user>/...` → XML multistatus liệt kê thư mục
  (app lấy mtime/etag để **tránh upload trùng**).
- `MKCOL /remote.php/dav/files/<user>/<thư-mục>` → tạo thư mục.
- `PUT /remote.php/dav/files/<user>/<path>` → upload **ảnh nhỏ** trực tiếp. Tôn
  trọng `X-OC-Mtime` (unixtime) giữ ngày chụp. **Ghi nguyên byte, tính SHA-256 khi ghi.**
- **Chunked upload v2** (video/ảnh lớn, chunk 5MB–5GB):
  - `MKCOL /remote.php/dav/uploads/<user>/<uuid>` + `Destination`
  - `PUT .../uploads/<user>/<uuid>/00001` (chunk số 1..10000, `Destination`, `OC-Total-Length`)
  - `MOVE .../uploads/<user>/<uuid>/.file` + `Destination` + `X-OC-Mtime` → ghép chunk → file đích.
  - `DELETE` thư mục upload để huỷ.

> Status code phải đúng: `201` (PUT/MKCOL mới), `204` (MOVE/ghi đè), `207` (PROPFIND).
> Sai → app coi là lỗi và thử lại vô hạn.

---

## 6. Thiết kế server Go

### Bố cục thư mục
```
nextcloud-server/
├── cmd/
│   ├── server/main.go          # khởi động, cấu hình cờ (port, data dir, PG DSN, Redis addr)
│   ├── verify/main.go          # quét ./data, so SHA-256 với Postgres, báo file hỏng/lệch
│   └── reindex/main.go         # quét ./data dựng lại bảng Postgres (phục hồi khi mất DB)
├── internal/
│   ├── compat/
│   │   ├── status.go           # /status.php
│   │   ├── capabilities.go     # /ocs/.../capabilities, /cloud/user
│   │   └── loginflow.go        # /index.php/login/v2, /login/v2/flow, /login/v2/poll
│   ├── webdav/
│   │   ├── dav.go              # PROPFIND/MKCOL/PUT/MOVE/DELETE (files/)
│   │   └── chunk.go            # chunking v2 (uploads/)
│   ├── auth/                   # app password + poll token (Redis)
│   ├── storage/                # ghi raw bytes + sha256 + mtime xuống ./data
│   ├── index/                  # bảng Postgres + lệnh reindex (quét lại đĩa)
│   └── dashboard/              # /dashboard: thumbnail + lọc ngày (embed.FS)
├── data/                       # ảnh backup RAW (gitignore)
├── go.mod
└── DESIGN.md
```

### Thư viện gợi ý
- **net/http** + `http.ServeMux` (Go 1.22+ method routing) — đủ, không cần framework.
- **golang.org/x/net/webdav** cho `/remote.php/dav/files`; phần `uploads/` (chunking) và compat layer **tự viết** (thư viện không biết chunking v2 + OCS).
- **PostgreSQL**: `jackc/pgx/v5` (driver tốt nhất cho Go). DB/schema **riêng**, không đụng Immich.
- **Redis**: `redis/go-redis/v9`. Dùng **DB index riêng** (vd `SELECT 3`) hoặc prefix key `photovault:` để không đụng key Immich.
- **Thumbnail**: `disintegration/imaging` (ảnh JPEG/PNG/HEIC*) — sinh thumbnail **chỉ cho dashboard**, lưu riêng `./data/.thumbs`, **không đụng file gốc**. (*HEIC có thể cần decoder riêng; nếu khó, dashboard hiển thị icon + metadata, mở full-res khi click.)
- **log/slog** để log handshake khi debug.

### Tái dùng hạ tầng Immich — ranh giới an toàn
- **PostgreSQL**: kết nối **cùng server**, nhưng **database riêng** (vd `photovault`). Tuyệt đối không ghi vào DB `immich`. Hạ tầng dùng chung, dữ liệu tách bạch.
- **Redis**: **cùng server**, khác **DB index** + prefix key riêng. Tránh `FLUSHALL`.
- Lý do: tiết kiệm tài nguyên (không dựng thêm Postgres/Redis) mà **không có nguy cơ** làm hỏng dữ liệu Immich.

---

## 6b. ĐẢM BẢO TOÀN VẸN ĐẦU–CUỐI (chương quan trọng nhất — chống hỏng file)

> Đây là lý do thật sự để tự viết server, và là thứ chữa đúng nỗi lo của bạn. Ngôn
> ngữ (Go hay PHP) không liên quan; **vòng verify checksum** mới quyết định "chắc ăn".

### Vấn đề cần phân biệt: hỏng ở đâu?
Một file ảnh có thể "hỏng" ở ba khâu khác nhau — phải biết khâu nào mới chữa đúng:
1. **Nguồn (iPhone/iOS đọc ảnh)**: iCloud "Optimize Storage" trả về bản **đã nén/thiếu**
   thay vì bản gốc; hoặc app đọc qua PHAsset nhận HEIC đã biến đổi. **Đây là nghi can số 1
   với Immich.** ⚠️ Lưu ý: **đổi sang app Nextcloud KHÔNG chữa được khâu này** vì nó cũng
   đọc ảnh qua đúng cơ chế iOS. Cách giảm thiểu: bật **"Download and Keep Originals"** trên
   iPhone (Settings → Photos) để iOS luôn có bản gốc trước khi upload.
2. **Đường truyền (upload)**: mạng ngắt giữa chừng, chunk ghép sai thứ tự → file cụt/lệch byte.
   **Server tự viết chữa được khâu này** bằng checksum + ghi atomic.
3. **Lưu trữ (đĩa)**: bit rot, đĩa lỗi theo thời gian. Chữa bằng **verify định kỳ** + có bản sao.

### 4 lớp phòng vệ server Go phải có

**Lớp 1 — Ghi atomic (không bao giờ để file ghi dở):**
- Ghi vào `*.part` (hoặc `.tmp`) trong cùng thư mục đích → `fsync` → `rename` sang tên thật.
- `rename` cùng filesystem là thao tác nguyên tử: hoặc có file đủ, hoặc không có gì.
- Chunked upload: chỉ `rename` file ghép cuối cùng sau khi **đủ mọi chunk** và kích thước khớp `OC-Total-Length`.

**Lớp 2 — Checksum khi ghi (bằng chứng toàn vẹn):**
- Tính **SHA-256** *trong lúc stream byte xuống đĩa* (dùng `io.MultiWriter(file, hasher)` — không đọc lại file, không tốn thêm I/O).
- Lưu vào Postgres: `path, original_name, size, sha256, taken_at (X-OC-Mtime), mime, uploaded_at`.
- Nếu app gửi kèm độ dài (`OC-Total-Length`) mà **size thực ghi ≠** → **từ chối**, không `rename`, trả lỗi để app thử lại.

**Lớp 3 — Lệnh `verify` (quét phát hiện hỏng về sau):**
- Duyệt mọi file trong `./data`, tính lại SHA-256, so với giá trị trong Postgres.
- Báo: file **lệch checksum** (bit rot / ghi hỏng), file **có trên đĩa thiếu trong DB**, file **có trong DB mất trên đĩa**.
- Chạy định kỳ (cron) → phát hiện bit rot sớm thay vì lúc cần mới biết.

**Lớp 4 — Lệnh `reindex` (phục hồi khi mất DB):**
- Quét `./data` dựng lại toàn bộ bảng Postgres từ chính các file.
- Khẳng định nguyên tắc: **file-trên-đĩa là source of truth**, Postgres chỉ là chỉ mục tra cứu.
- Mất Postgres/Redis → ảnh vẫn nguyên; chạy `reindex` là có lại index.

### (Khuyến nghị mạnh) Đối chiếu checksum với bản trên iPhone
Để **chứng minh** file về máy đúng bằng file trên iPhone (chống đúng khâu 1 & 2):
- Dùng app **Nextcloud iOS vẫn có phần Files**: không tiện lấy hash phía iPhone trực tiếp.
- Cách thực dụng: sau mỗi đợt backup, **mở ngẫu nhiên vài ảnh/video** trên dashboard và so
  bằng mắt + kích thước với trên máy; định kỳ **cắm cáp copy một mẫu DCIM** rồi so SHA-256
  mẫu đó với bản server (`sha256sum` hai bên). Khớp → tin cậy cả luồng; lệch → nghi khâu 1 (iOS).
- Đây là "kiểm toán điểm" rẻ tiền nhưng đủ để bạn **yên tâm có cơ sở**, không còn "cảm giác".

### Ghi chú thành thật về nhược điểm còn lại
- **Handshake Nextcloud dễ vỡ** khi app iOS cập nhật đổi hành vi → coi đây là chi phí bảo trì
  định kỳ. Giảm thiểu: **log mọi request** của app (§9) và giữ một **Nextcloud PHP chạy Docker**
  để đối chiếu mẫu response đúng khi có trục trặc.
- Đây là lý do phải có §6b vững: dù handshake có trục trặc, **lớp toàn vẹn vẫn đảm bảo**
  thứ gì đã ghi xuống đĩa là đúng byte — không bao giờ "hỏng âm thầm".

---

## 7. HTTPS trên iOS (đã chốt dùng HTTPS)

iOS có **App Transport Security (ATS)** rất khó chịu với cert. Với server LAN theo IP
(`192.168.1.55`), hai cách khả thi:

1. **Reverse proxy Caddy (khuyến nghị)**: Caddy đứng trước server Go, tự quản TLS.
   Với IP nội bộ, dùng **internal CA của Caddy**; export root CA của Caddy và **cài +
   tin cậy** nó trên iPhone một lần. Server Go chỉ cần nói HTTP với Caddy ở localhost.
2. **Go tự chạy TLS**: sinh cert tự-ký **có SAN = IP `192.168.1.55`** (bắt buộc, iOS
   yêu cầu IP nằm trong SAN, không chấp CN). Cài cert vào iPhone:
   - Settings → General → VPN & Device Management → cài profile cert.
   - Settings → General → About → Certificate Trust Settings → **bật trust đầy đủ**.
   - Cert nên có thời hạn **≤ 825 ngày** (iOS từ chối cert tự-ký dài hơn).

> Trong Login Flow, `server` và `poll.endpoint` trả về **phải là `https://192.168.1.55:8081`**
> khớp đúng cert, nếu không app sẽ lỗi TLS hoặc nối sai URL.

---

## 8. Lộ trình làm (tăng dần, mỗi bước kiểm chứng được)

| Mốc | Nội dung | Cách kiểm chứng |
|---|---|---|
| **M0** | `main.go` + HTTPS + `/status.php` + `/dashboard` trống + kết nối PG/Redis | `curl -k https://.../status.php` trả JSON; app ping được |
| **M1** | Capabilities + `/cloud/user` | app **qua được** màn hình nhập địa chỉ |
| **M2** | Login Flow v2 đầy đủ (flow page + poll, token trong Redis) | đăng nhập được từ **app iOS thật** |
| **M3** | WebDAV `PROPFIND`/`MKCOL`/`PUT` + `X-OC-Mtime` + SHA-256 + index Postgres | ảnh **nhỏ** vào `./data` đúng byte, đúng ngày, có checksum trong DB |
| **M4** | Chunked upload v2 (`uploads/`, `MOVE .file`) | **video/ảnh lớn** ghép đúng kích thước, SHA-256 khớp |
| **M5** | Dashboard thumbnail + lọc ngày | mở `/dashboard` duyệt ảnh theo ngày, có thumbnail |
| **M6** | `verify` + `reindex` + chống trùng (etag/mtime) + ghi atomic | ngắt mạng giữa chừng rồi bật lại: không mất/trùng; `verify` báo toàn vẹn |

> Nguyên tắc: **không nhảy mốc sau khi mốc trước chưa chạy với app iOS thật.**
> Phần khó nhất (M1–M2) debug bằng cách **log mọi request** từ app.

---

## 9. Mẹo debug handshake
- Log **toàn bộ** method + path + header + body mọi request đến → biết chính xác app gọi gì, thứ tự nào.
- Endpoint trả sai → app **im lặng thất bại** hoặc **thử lại liên tục**; nhìn log lặp là ra.
- Chạy tạm **Nextcloud PHP bằng Docker** để xem **mẫu response đúng**, rồi bắt chước trong Go.

---

## 10. Phạm vi KHÔNG làm (giữ nhẹ + raw)
- Không transcode/nén/sửa ảnh gốc (chỉ thumbnail riêng cho dashboard).
- Không sharing, link public, multi-user thật.
- Không sync hai chiều, không xoá ảnh từ app xuống server.
- Không CalDAV/CardDAV/Talk/Notes, không quota thật.
- Không đụng database/key của Immich.

---

## 11. Quyết định đã chốt
1. Client → **app Nextcloud iOS** (client miễn phí + auto-upload nền duy nhất thỏa 4 ràng buộc — §1b).
2. Metadata → **PostgreSQL** (instance Immich, DB riêng).
3. Token/cache → **Redis** (instance Immich, DB index/prefix riêng).
4. Dashboard → **thumbnail + lọc ngày**.
5. Transport → **HTTPS** (Caddy internal CA hoặc cert tự-ký có SAN=IP, tin cậy trên iOS).
6. Toàn vẹn → **ghi atomic + SHA-256 mỗi file + verify + reindex** (§6b); file-trên-đĩa là source of truth.
7. Go được chọn vì **kiểm soát code + dễ nhúng verify + nhẹ**, KHÔNG vì tin "PHP làm hỏng file".

→ Sẵn sàng dựng khung M0–M1 để thử ngay với app iOS.
