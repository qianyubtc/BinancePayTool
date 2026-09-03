#!/usr/bin/env python3
"""跨语言端到端测试：真实网关二进制 × Python SDK × mock 币安。

用法: python3 e2e/cross_e2e.py [网关二进制路径，默认 server/bpaygate]
流程: 起 mock 币安与回调接收器 → 起网关子进程 → SDK 下单 → 注入到账流水
      → 等回调并验签 → 查单断言 paid → 再测回填(claim)路径。
"""
import json
import os
import queue
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk", "python"))
from bpaygate import BPayGate

SECRET = "e2e-secret-0123456789abcdef"
TXNS = []
CB_QUEUE = queue.Queue()


class MockBinance(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/sapi/v1/pay/transactions"):
            body = json.dumps({"code": "000000", "message": "success", "data": TXNS, "success": True}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, *a):
        pass


class CallbackReceiver(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        CB_QUEUE.put((dict(self.headers), body))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *a):
        pass


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def wait_http(url, seconds=15):
    for _ in range(seconds * 10):
        try:
            urllib.request.urlopen(url, timeout=1)
            return True
        except Exception:
            time.sleep(0.1)
    return False


def main():
    binary = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "..", "server", "bpaygate")
    binary = os.path.abspath(binary)
    if not os.path.isfile(binary):
        sys.exit(f"找不到网关二进制: {binary}（先在 server/ 下 go build -o bpaygate .）")

    mock = ThreadingHTTPServer(("127.0.0.1", 0), MockBinance)
    threading.Thread(target=mock.serve_forever, daemon=True).start()
    cbs = ThreadingHTTPServer(("127.0.0.1", 0), CallbackReceiver)
    threading.Thread(target=cbs.serve_forever, daemon=True).start()

    gw_port = free_port()
    base = f"http://127.0.0.1:{gw_port}"
    cfg = tempfile.NamedTemporaryFile("w", suffix=".env", delete=False)
    cfg.write(f"""LISTEN=127.0.0.1:{gw_port}
BASE_URL={base}
API_AUTH_KEY={SECRET}
BINANCE_API_KEY=k
BINANCE_API_SECRET=s
BINANCE_UID=90000001
BINANCE_API_BASE=http://127.0.0.1:{mock.server_address[1]}
CURRENCIES=USDT,USDC
POLL_INTERVAL=1
DB_PATH={tempfile.mkdtemp()}/e2e.db
""")
    cfg.close()
    proc = subprocess.Popen([binary, "-config", cfg.name], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    try:
        assert wait_http(base + "/healthz"), "网关未就绪"
        gw = BPayGate(base, SECRET)
        cb_url = f"http://127.0.0.1:{cbs.server_address[1]}/notify"

        # ① 唯一金额自动匹配
        order = gw.create_order("E2E-1", "12", currency="USDT", callback_url=cb_url, return_url="https://shop.test/done")
        assert order["status"] == "pending" and order["pay_amount"] != "12", order
        assert order["pay_amount"].startswith("12.00"), order  # 首单应落在最小尾数档 0.0001–0.0099
        TXNS.append({"orderId": "452100000000000001", "note": "", "orderType": "C2C",
                     "transactionId": "P_E2E000001", "transactionTime": int(time.time() * 1000),
                     "amount": order["pay_amount"], "currency": "USDT",
                     "counterpartyId": 1163000000, "payerInfo": {"name": "e2e-payer"}})
        headers, body = CB_QUEUE.get(timeout=15)
        payload = BPayGate.verify_callback(headers, body, SECRET)
        assert payload["event"] == "paid" and payload["merchant_order_id"] == "E2E-1", payload
        got = gw.get_order(order["order_id"])
        assert got["status"] == "paid" and got["matched_by"] == "amount", got
        got2 = gw.get_order_by_merchant_id("E2E-1")
        assert got2["order_id"] == order["order_id"]
        print("① 金额匹配 + 回调验签 ... PASS")

        # ② 回填 claim
        order2 = gw.create_order("E2E-2", "33", currency="USDT", callback_url=cb_url)
        TXNS.append({"orderId": "452100000000000002", "note": "", "orderType": "C2C",
                     "transactionId": "P_E2E000002", "transactionTime": int(time.time() * 1000),
                     "amount": "33", "currency": "USDT",
                     "counterpartyId": 1163000000, "payerInfo": {"name": "e2e-payer"}})
        token = order2["pay_url"].rsplit("/", 1)[1]
        req = urllib.request.Request(base + f"/pay/{token}/claim",
                                     data=json.dumps({"binance_order_id": "452100000000000002"}).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
        out = json.loads(urllib.request.urlopen(req, timeout=10).read())
        assert out["data"]["code"] == "OK" and out["data"]["status"] == "paid", out
        headers, body = CB_QUEUE.get(timeout=15)
        payload = BPayGate.verify_callback(headers, body, SECRET)
        assert payload["event"] == "paid" and payload["matched_by"] == "claim", payload
        print("② 回填核验 + 回调验签 ... PASS")

        # ③ 收银页可渲染
        html = urllib.request.urlopen(base + "/pay/" + token, timeout=5).read().decode()
        assert "币安" in html and order2["note_code"] in html
        # 页面 JS 轮询的状态接口必须真实可达（相对路径曾导致 /pay/status 404）
        st = json.loads(urllib.request.urlopen(base + f"/pay/{token}/status", timeout=5).read())
        assert st["status"] == "paid", st
        print("③ 收银页渲染 ... PASS")
        print("CROSS-E2E ALL PASS")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        os.unlink(cfg.name)


if __name__ == "__main__":
    main()
