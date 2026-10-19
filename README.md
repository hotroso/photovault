# photovault

Server Go **tương thích client Nextcloud**, phục vụ **một mục đích duy nhất**:
backup RAW toàn bộ ảnh/video từ iPhone về máy tính, dùng app Nextcloud iOS chính
thức (miễn phí) làm client. Thiết kế đầy đủ: xem [`DESIGN.md`](./DESIGN.md).

> Đây KHÔNG phải sản phẩm của Nextcloud GmbH. Nó chỉ nói đúng giao thức mà client
> Nextcloud mong đợi, để tái dùng app iOS miễn phí làm công cụ auto-upload.

## Trạng thái

Đang ở mốc **M0–M1**: handshake phát hiện server (`/status.php`, capabilities,
user) + khung dashboard. Chưa có đăng nhập (M2), chưa có upload WebDAV (M3–M4).

## Triển khai nhanh trên Ubuntu (khuyến nghị)

```bash
chmod +x deploy.sh
./deploy.sh            # tidy + vet + build 3 binary rồi chạy server
```

Lần đầu chạy, nếu chưa có `.env`, script tự tạo từ `.env.example` và dừng lại để
bạn điền giá trị thật (PG DSN, Redis, IP/cổng). Điền xong chạy lại `./deploy.sh`.

Các lệnh con:

```bash
./deploy.sh build            # chỉ build, không chạy
./deploy.sh run              # chạy server (build nếu cần)
./deploy.sh verify           # kiểm tra toàn vẹn (so SHA-256 đĩa vs Postgres)
./deploy.sh reindex          # dựng lại index Postgres từ đĩa
./deploy.sh install-service  # cài systemd service: chạy nền + tự khởi động
```

## Chạy thủ công (dev)

```bash
cp .env.example .env     # rồi điền giá trị (PG DSN, Redis, IP/cổng...)
go run ./cmd/server
```

Kiểm chứng M0–M1:

```bash
curl -k https://192.168.1.55:8081/status.php
curl -k -H "OCS-APIRequest: true" https://192.168.1.55:8081/ocs/v2.php/cloud/capabilities
curl -k -H "OCS-APIRequest: true" https://192.168.1.55:8081/ocs/v2.php/cloud/user
```

Mở dashboard: `https://192.168.1.55:8081/dashboard`

## TLS trên Ubuntu

Hai cách (chi tiết trong `DESIGN.md` §7):

1. **Caddy reverse proxy** (khuyến nghị): dùng `Caddyfile.example`. Server Go chạy
   HTTP ở `127.0.0.1:8080`, Caddy lo TLS ở `:8081`.
2. **Go tự chạy TLS**: sinh cert tự ký có SAN = IP, điền `APP_TLS_CERT` /
   `APP_TLS_KEY` trong `.env`.

Cả hai cách đều cần **cài + tin cậy cert trên iPhone**.

## Bố cục

```
cmd/server/         # entrypoint
internal/
  config/           # nạp .env
  httpx/            # logging middleware + helper JSON/OCS
  compat/           # giả lập Nextcloud: status, capabilities, user (M2: login flow)
  dashboard/        # trang xem ảnh
  server/           # router + vòng đời server
reference/          # mẫu handshake + source Nextcloud thật để đối chiếu (không commit)
```
