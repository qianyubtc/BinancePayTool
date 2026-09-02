# BinancePayTool 接入协议 v1

> 签名头与错误码沿用 `X-BPG-` / `BPG` 前缀，它们是协议稳定标识，与仓库名无关。

所有接口均为 HTTP + JSON（UTF-8）。金额一律为**字符串**十进制（如 `"10.0037"`），最多 8 位小数。
时间戳一律为**毫秒** Unix 时间。

## 1. 签名

商户与网关共享一个密钥 `API_AUTH_KEY`（下称 `secret`），同时用于「商户调用网关」与「网关回调商户」。

### 1.1 商户 → 网关（API 请求签名）

请求头：

| Header | 说明 |
|---|---|
| `X-BPG-Timestamp` | 当前毫秒时间戳；与服务器偏差超过 300 秒拒绝 |
| `X-BPG-Nonce` | 随机串（≥16 字符）；5 分钟内不得重复 |
| `X-BPG-Signature` | 小写十六进制签名 |

签名串（`\n` 为换行符；`path` 只含路径不含查询串与域名；GET/无 body 时 body 视为空串）：

```
string_to_sign = timestamp + "\n" + nonce + "\n" + METHOD + "\n" + path + "\n" + SHA256_HEX(body)
signature      = HMAC_SHA256_HEX(secret, string_to_sign)
```

### 1.2 网关 → 商户（回调签名）

回调为 `POST callback_url`，`Content-Type: application/json`，请求头同上三个，签名串少 METHOD 与 path：

```
string_to_sign = timestamp + "\n" + nonce + "\n" + SHA256_HEX(body)
```

商户校验：时间戳偏差 ≤300 秒、签名一致（常数时间比较）。校验通过返回 HTTP 2xx 即视为成功；
否则网关按 `0s, 15s, 60s, 5m, 15m, 1h, 3h` 重试 7 次后放弃。**回调可能重复投递，商户须按
`order_id` 幂等处理。**

## 2. 接口

### 2.1 创建订单 `POST /api/v1/orders`

请求体：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `merchant_order_id` | string | 是 | 商户侧唯一订单号（≤64 字符）；重复返回 `ERR_DUPLICATE` |
| `currency` | string | 是 | 币种，须在网关 `CURRENCIES` 白名单内（如 `USDT`）|
| `amount` | string | 是 | 基础金额，>0，小数位 ≤ 网关 `AMOUNT_DECIMALS` |
| `callback_url` | string | 否 | 支付结果回调地址 |
| `return_url` | string | 否 | 支付成功后收银页跳转地址（自动附加 `?order_id=&status=`）|
| `timeout` | int | 否 | 有效期秒数，120–86400，缺省用网关 `ORDER_TTL` |

响应 `data`：

| 字段 | 说明 |
|---|---|
| `order_id` | 网关订单号 |
| `merchant_order_id` | 原样返回 |
| `status` | `pending` |
| `currency` / `base_amount` / `pay_amount` | `pay_amount` 为**实际应付的唯一金额**，付款方必须严格按它支付 |
| `note_code` | 6 位备注码；付款方转账备注里带上它可加速/兜底确认（可选）|
| `pay_url` | 网关托管收银页地址，直接跳转即可 |
| `receive_uid` | 收款方币安 UID |
| `created_at` / `expires_at` | 毫秒时间戳 |

### 2.2 查询订单 `GET /api/v1/orders/{order_id}`

响应 `data` 在 2.1 基础上增加：`actual_amount`、`paid_at`、`matched_by`（`amount`/`note`/`claim`）、
`binance_order_id`（币安 18 位订单编号）、`payer_id`（付款方 Pay 账户 ID）、`overpaid`（bool）。
也可用 `GET /api/v1/orders/by-merchant/{merchant_order_id}` 按商户单号查询。

### 2.3 关闭订单 `POST /api/v1/orders/{order_id}/close`

仅 `pending` 可关闭；成功后状态为 `closed`，唯一金额进入冷却。

### 2.4 收银页（免签名，公开）

- `GET /pay/{token}`：托管收银页（token 含在 `pay_url` 中）
- `GET /pay/{token}/status`：`{status, paid_at, return_url}`，页面轮询用
- `POST /pay/{token}/claim`：`{"binance_order_id":"18位数字"}`——「我已付款」回填入口，限流 5 次/分钟

### 2.5 响应外层

```json
{"code":"OK","data":{...}}
{"code":"ERR_AUTH","message":"..."}
```

`code` 取值：`OK`、`ERR_AUTH`、`ERR_PARAM`、`ERR_DUPLICATE`、`ERR_NOT_FOUND`、`ERR_STATE`、
`ERR_RATE_LIMIT`、`ERR_ALLOC`（唯一金额耗尽）、`ERR_INTERNAL`。

## 3. 回调体

```json
{
  "event": "paid",                    // paid | underpaid | expired
  "order_id": "…",
  "merchant_order_id": "…",
  "status": "paid",
  "currency": "USDT",
  "base_amount": "10",
  "pay_amount": "10.0037",
  "actual_amount": "10.0037",
  "overpaid": false,
  "matched_by": "amount",
  "binance_order_id": "452021922068888888",
  "binance_txn_id": "P_EXAMPLE1234567890",
  "payer_id": "1160000000",
  "paid_at": 1788325910559,
  "timestamp": 1788325912000
}
```

`underpaid`：备注/回填命中但实付低于基础金额，需人工处理；`expired`：超时未支付。

## 4. 订单状态机

```
pending ──精确金额/备注码/回填命中──▶ paid
pending ──实付不足(备注/回填)──▶ underpaid
pending ──到期──▶ expired ──宽限期内精确金额到账/回填──▶ paid
pending ──商户关闭──▶ closed
```

## 5. 匹配规则（网关内部，供理解）

对每条**入账**流水（金额>0、币种在白名单、币安 `orderId` 未被消费过）：

1. 金额与某待支付订单的 `pay_amount` 完全相等，且到账时间在订单窗口内 → 自动确认；
2. 否则备注含某订单 `note_code`（不区分大小写）→ 实付 ≥ 基础金额则确认，否则 `underpaid`；
3. 否则保留，等付款方在收银页回填币安订单编号（7 天内有效）。

同一笔币安流水只能核销一个订单（以币安 `orderId` 幂等）。
