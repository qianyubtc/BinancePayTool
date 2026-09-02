"""BPayGate Python SDK（仅标准库，Python 3.7+）

用法:
    from bpaygate import BPayGate

    gw = BPayGate("https://pay.example.com", "你的API_AUTH_KEY")
    order = gw.create_order("SHOP-1001", "10", currency="USDT",
                            callback_url="https://shop.example.com/bpg/notify",
                            return_url="https://shop.example.com/orders/1001")
    # 跳转用户到 order["pay_url"]

    # 回调处理（Flask 示例见 examples/python/app.py）:
    payload = BPayGate.verify_callback(request.headers, request.get_data(), SECRET)
    if payload["event"] == "paid": ...  # 按 merchant_order_id 幂等发货
"""
import hashlib
import hmac
import json
import secrets
import time
import urllib.error
import urllib.request

__all__ = ["BPayGate", "BPayGateError"]


class BPayGateError(Exception):
    def __init__(self, code, message="", http_status=0):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.http_status = http_status


def _sha256_hex(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def _hmac_hex(secret: str, msg: str) -> str:
    return hmac.new(secret.encode(), msg.encode(), hashlib.sha256).hexdigest()


def sign_request(secret, timestamp, nonce, method, path, body: bytes) -> str:
    sts = "\n".join([timestamp, nonce, method, path, _sha256_hex(body)])
    return _hmac_hex(secret, sts)


def sign_callback(secret, timestamp, nonce, body: bytes) -> str:
    sts = "\n".join([timestamp, nonce, _sha256_hex(body)])
    return _hmac_hex(secret, sts)


class BPayGate:
    def __init__(self, base_url: str, secret: str, timeout: int = 10):
        self.base_url = base_url.rstrip("/")
        self.secret = secret
        self.timeout = timeout

    # ---- API ----

    def create_order(self, merchant_order_id: str, amount: str, currency: str = "USDT",
                     callback_url: str = None, return_url: str = None, timeout: int = None) -> dict:
        payload = {"merchant_order_id": merchant_order_id, "amount": str(amount), "currency": currency}
        if callback_url:
            payload["callback_url"] = callback_url
        if return_url:
            payload["return_url"] = return_url
        if timeout:
            payload["timeout"] = int(timeout)
        return self._request("POST", "/api/v1/orders", payload)

    def get_order(self, order_id: str) -> dict:
        return self._request("GET", "/api/v1/orders/" + order_id)

    def get_order_by_merchant_id(self, merchant_order_id: str) -> dict:
        return self._request("GET", "/api/v1/orders/by-merchant/" + merchant_order_id)

    def close_order(self, order_id: str) -> dict:
        return self._request("POST", "/api/v1/orders/" + order_id + "/close", {})

    # ---- 回调验签 ----

    @staticmethod
    def verify_callback(headers, body: bytes, secret: str, max_skew_ms: int = 300000) -> dict:
        """校验网关回调。headers 可传 dict 或任意支持 get 的对象（大小写不敏感）。
        校验通过返回回调 JSON；失败抛 BPayGateError。商户须按 merchant_order_id 幂等处理。"""
        def h(name):
            for k in (name, name.lower(), name.upper()):
                v = headers.get(k) if hasattr(headers, "get") else None
                if v:
                    return v
            return ""
        ts, nonce, sig = h("X-Bpg-Timestamp") or h("X-BPG-Timestamp"), h("X-Bpg-Nonce") or h("X-BPG-Nonce"), \
            h("X-Bpg-Signature") or h("X-BPG-Signature")
        if not ts or not nonce or not sig:
            raise BPayGateError("ERR_AUTH", "缺少签名头")
        try:
            skew = abs(int(time.time() * 1000) - int(ts))
        except ValueError:
            raise BPayGateError("ERR_AUTH", "时间戳非法")
        if skew > max_skew_ms:
            raise BPayGateError("ERR_AUTH", "时间戳偏差过大")
        if not isinstance(body, bytes):
            body = body.encode()
        if not hmac.compare_digest(sign_callback(secret, ts, nonce, body), sig):
            raise BPayGateError("ERR_AUTH", "签名错误")
        return json.loads(body.decode())

    # ---- 内部 ----

    def _request(self, method: str, path: str, payload: dict = None) -> dict:
        body = json.dumps(payload, separators=(",", ":")).encode() if payload is not None else b""
        ts = str(int(time.time() * 1000))
        nonce = secrets.token_hex(12)
        req = urllib.request.Request(self.base_url + path, data=body if body else None, method=method)
        req.add_header("Content-Type", "application/json")
        req.add_header("X-BPG-Timestamp", ts)
        req.add_header("X-BPG-Nonce", nonce)
        req.add_header("X-BPG-Signature", sign_request(self.secret, ts, nonce, method, path, body))
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                out = json.loads(resp.read().decode())
        except urllib.error.HTTPError as e:
            try:
                out = json.loads(e.read().decode())
            except Exception:
                raise BPayGateError("ERR_HTTP", f"HTTP {e.code}", e.code)
            raise BPayGateError(out.get("code", "ERR_HTTP"), out.get("message", ""), e.code)
        except urllib.error.URLError as e:
            raise BPayGateError("ERR_NETWORK", str(e))
        if out.get("code") != "OK":
            raise BPayGateError(out.get("code", "ERR_UNKNOWN"), out.get("message", ""))
        return out["data"]
