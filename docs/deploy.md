# 部署指南（VPS + Cloudflare 域名）

目标：`https://pay.你的域名/` 对外提供收银页、体验页与商户 API。

## 0. 选 VPS 的唯一硬要求：能直连币安 API

币安对部分地区 IP（如美国）返回 `451 Service unavailable from a restricted location`。
香港、新加坡、日本等机房通常可用。开机后先验：

```bash
curl -s https://api.binance.com/api/v3/ping && echo OK
```

配置 1 核 1G 足够；网关是单进程、SQLite、每 3–4 秒一次 API 调用。

## 1. 安装

```bash
# 在 VPS 上（Debian/Ubuntu 为例）
sudo useradd -r -s /usr/sbin/nologin bpg
sudo mkdir -p /opt/bpaygate && sudo chown bpg:bpg /opt/bpaygate
# 方式一：本机交叉编译后上传
#   cd server && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bpaygate-linux .
#   scp bpaygate-linux vps:/opt/bpaygate/bpaygate
# 方式二：VPS 上有 Go 直接 git clone 后 go build
```

## 2. 配置

```bash
sudo -u bpg cp config.example.env /opt/bpaygate/config.env
sudo -u bpg /opt/bpaygate/bpaygate -gen-key   # 得到 API_AUTH_KEY
sudo -u bpg nano /opt/bpaygate/config.env
sudo chmod 600 /opt/bpaygate/config.env
```

必填：`BASE_URL=https://pay.你的域名`、`API_AUTH_KEY`、`BINANCE_API_KEY/SECRET`（只读，**绑定 VPS 出口 IP**）、`BINANCE_UID`。
对外体验建议：`DEMO_ENABLED=true`、`DEMO_AMOUNTS=0.5,1`、`RECEIVE_LINK=<收款二维码解码内容>`（见下）。

### 拿到 RECEIVE_LINK

币安 App → 支付 → 收款 → 保存二维码，用任意扫码工具读出内容，通常是 `https://app.binance.com/...` 形式的链接。
填入后网关会：① 服务端生成同样的二维码给桌面端扫；② 在手机端收银页放「打开币安 App」按钮直接唤起。
不填则只提供手动转账模式（或用 `QR_IMAGE` 指向二维码图片文件，仅提供扫码）。

## 3. 起服务

```bash
sudo cp deploy/bpaygate.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now bpaygate
sudo journalctl -u bpaygate -f      # 看到「监听 0.0.0.0:8080」即正常
```

网关只监听本机即可（`LISTEN=127.0.0.1:8080`），对外由下面的入口层负责 HTTPS。

## 4. 对外 HTTPS（二选一）

**方案 B · Cloudflare Tunnel（推荐）**：不开公网端口、证书全托管、VPS 防火墙可以全关。
按 `deploy/cloudflared.yml` 头部注释五步走完，Cloudflare DNS 里会自动出现 `pay.你的域名` 的 CNAME。

**方案 A · Caddy**：`deploy/Caddyfile` 改域名后 `caddy run`，Cloudflare DNS 记录开代理时把 SSL/TLS 设为 **Full (strict)**。

## 5. 上线检查

- `curl https://pay.你的域名/healthz` 返回 `{"ok":true}`
- 打开 `https://pay.你的域名/demo`，手机与桌面各走一遍
- 币安后台确认 API Key 已绑定 VPS IP、仅读取权限
- 定期把收款账户余额转走，收款专号不放大额

## 6. 升级

替换二进制后 `sudo systemctl restart bpaygate`。数据库 schema 自动迁移（`CREATE IF NOT EXISTS`）。
