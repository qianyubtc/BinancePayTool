"""签名向量测试：python3 sdk/python/test_bpaygate.py"""
import json
import os
import sys
import time
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from bpaygate import BPayGate, BPayGateError, sign_callback, sign_request

VEC = os.path.join(os.path.dirname(__file__), "..", "..", "docs", "test_vectors.json")


class TestVectors(unittest.TestCase):
    def setUp(self):
        with open(VEC, encoding="utf-8") as f:
            self.vf = json.load(f)

    def test_vectors(self):
        for i, c in enumerate(self.vf["cases"]):
            if c["type"] == "request":
                got = sign_request(self.vf["secret"], c["timestamp"], c["nonce"], c["method"], c["path"], c["body"].encode())
            else:
                got = sign_callback(self.vf["secret"], c["timestamp"], c["nonce"], c["body"].encode())
            self.assertEqual(got, c["signature"], f"case {i} ({c['type']})")

    def test_verify_callback(self):
        secret = self.vf["secret"]
        body = b'{"event":"paid","merchant_order_id":"M1"}'
        ts = str(int(time.time() * 1000))
        nonce = "n" * 20
        headers = {
            "X-Bpg-Timestamp": ts,
            "X-Bpg-Nonce": nonce,
            "X-Bpg-Signature": sign_callback(secret, ts, nonce, body),
        }
        payload = BPayGate.verify_callback(headers, body, secret)
        self.assertEqual(payload["event"], "paid")
        headers["X-Bpg-Signature"] = "bad"
        with self.assertRaises(BPayGateError):
            BPayGate.verify_callback(headers, body, secret)
        # 过期时间戳
        old = {"X-Bpg-Timestamp": "1000000", "X-Bpg-Nonce": nonce,
               "X-Bpg-Signature": sign_callback(secret, "1000000", nonce, body)}
        with self.assertRaises(BPayGateError):
            BPayGate.verify_callback(old, body, secret)


if __name__ == "__main__":
    unittest.main(verbosity=2)
