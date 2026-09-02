#!/usr/bin/env python3
"""最小商户示例（仅标准库）。

    export BPG_BASE=http://127.0.0.1:8080 BPG_SECRET=你的API_AUTH_KEY
    python3 app.py   # 然后浏览器打开 http://127.0.0.1:9000/buy
"""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "sdk", "python"))
from bpaygate import BPayGate, BPayGateError

BASE = os.environ.get("BPG_BASE", "http://127.0.0.1:8080")
SECRET = os.environ.get("BPG_SECRET", "")
PORT = int(os.environ.get("PORT", "9000"))
gw = BPayGate(BASE, SECRET)


class Shop(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/buy"):
            mid = f"DEMO-{int(time.time())}"
            try:
                order = gw.create_order(
                    mid, "1", currency="USDT",
                    callback_url=f"http://127.0.0.1:{PORT}/notify",
                    return_url=f"http://127.0.0.1:{PORT}/done",
                )
            except BPayGateError as e:
                self.send_response(500)
                self.end_headers()
                self.wfile.write(str(e).encode())
                return
            self.send_response(302)
            self.send_header("Location", order["pay_url"])
            self.end_headers()
        elif self.path.startswith("/done"):
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write("<h1>支付完成，感谢购买！</h1>".encode())
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        if self.path != "/notify":
            self.send_response(404)
            self.end_headers()
            return
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        try:
            payload = BPayGate.verify_callback(self.headers, body, SECRET)
        except BPayGateError as e:
            print("回调验签失败:", e)
            self.send_response(401)
            self.end_headers()
            return
        # TODO: 按 merchant_order_id 幂等发货
        print("收到回调:", payload["event"], payload["merchant_order_id"], payload.get("actual_amount"))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print(f"商户示例: http://127.0.0.1:{PORT}/buy")
    ThreadingHTTPServer(("127.0.0.1", PORT), Shop).serve_forever()
