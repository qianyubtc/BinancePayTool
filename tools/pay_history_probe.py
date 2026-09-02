#!/usr/bin/env python3
"""
币安支付（Binance Pay）流水探测脚本 —— 验证“个人账户 + 官方只读 API”方案是否可行。

调用官方接口: GET /sapi/v1/pay/transactions  (签名接口, 权重 3000/UID, 默认最近 90 天)
文档: https://developers.binance.com/docs/pay/rest-api

用法（二选一）:
  A. 环境变量:
     export BINANCE_API_KEY=你的APIKey
     export BINANCE_API_SECRET=你的Secret     # 只需勾选 "Enable Reading"，不要开交易/提现权限
     export BINANCE_UID=你的币安UID           # 可选，用于判定收/付方向
  B. 密钥文件（推荐，密钥不经过任何对话/终端历史）:
     ~/.binance_pay_probe.env  内容三行:  BINANCE_API_KEY=...  BINANCE_API_SECRET=...  BINANCE_UID=...
     chmod 600 ~/.binance_pay_probe.env
     （可用 BINANCE_PROBE_ENV=/path/to/file 指定其他路径）

  python3 tools/pay_history_probe.py --days 30          # 打印最近 30 天 Pay 流水
  python3 tools/pay_history_probe.py --watch 5          # 每 5 秒轮询，打印新出现的收款

脚本从不打印密钥；测完请在币安后台删除该 API Key 并删掉密钥文件。

只用标准库，不依赖第三方包。
"""
from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime

BASE_URLS = [
    "https://api.binance.com",
    "https://api1.binance.com",
    "https://api-gcp.binance.com",
]

WALLET_NAMES = {1: "Funding", 2: "Spot", 3: "Fiat", 4: "Card", 5: "Earn", 6: "Card"}

DEFAULT_ENV_FILE = os.path.expanduser("~/.binance_pay_probe.env")


def load_env_file() -> None:
    """若环境变量缺失，则从密钥文件读取 KEY=VALUE 行；不打印任何值。"""
    path = os.environ.get("BINANCE_PROBE_ENV", DEFAULT_ENV_FILE)
    if os.environ.get("BINANCE_API_KEY") and os.environ.get("BINANCE_API_SECRET"):
        return
    if not os.path.isfile(path):
        return
    mode = os.stat(path).st_mode & 0o777
    if mode & 0o077:
        print(f"[warn] {path} 权限为 {oct(mode)}，建议 chmod 600", file=sys.stderr)
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            k, v = k.strip(), v.strip().strip('"').strip("'")
            if k in ("BINANCE_API_KEY", "BINANCE_API_SECRET", "BINANCE_UID") and v:
                os.environ.setdefault(k, v)


def signed_get(path: str, params: dict, key: str, secret: str) -> dict:
    params = dict(params)
    params["timestamp"] = int(time.time() * 1000)
    params.setdefault("recvWindow", 10000)
    query = urllib.parse.urlencode(params)
    sig = hmac.new(secret.encode(), query.encode(), hashlib.sha256).hexdigest()
    last_err = None
    for base in BASE_URLS:
        url = f"{base}{path}?{query}&signature={sig}"
        req = urllib.request.Request(url, headers={"X-MBX-APIKEY": key})
        try:
            with urllib.request.urlopen(req, timeout=15) as resp:
                return json.loads(resp.read().decode())
        except urllib.error.HTTPError as e:
            body = e.read().decode(errors="replace")
            # 4xx 是账号/参数/权限问题，换域名没意义，直接抛出
            if 400 <= e.code < 500:
                raise SystemExit(f"HTTP {e.code} from {base}: {body}")
            last_err = f"HTTP {e.code} from {base}: {body}"
        except (urllib.error.URLError, TimeoutError) as e:
            last_err = f"{base}: {e}"
    raise SystemExit(f"所有接入点均失败: {last_err}")


def fetch_pay_transactions(key, secret, start_ms=None, end_ms=None, limit=100):
    params = {"limit": limit}
    if start_ms is not None:
        params["startTime"] = start_ms
    if end_ms is not None:
        params["endTime"] = end_ms
    data = signed_get("/sapi/v1/pay/transactions", params, key, secret)
    if not data.get("success", False) and data.get("code") != "000000":
        raise SystemExit(f"接口返回失败: {json.dumps(data, ensure_ascii=False)}")
    return data.get("data", [])


def direction(tx: dict, my_uid: str | None) -> str:
    recv = (tx.get("receiverInfo") or {}).get("binanceId")
    payer = (tx.get("payerInfo") or {}).get("binanceId")
    if my_uid:
        if str(recv) == my_uid:
            return "收款 IN "
        if str(payer) == my_uid:
            return "付款 OUT"
    amt = str(tx.get("amount", ""))
    if amt.startswith("-"):
        return "付款 OUT"
    return "?      "


def fmt(tx: dict, my_uid: str | None) -> str:
    ts = datetime.fromtimestamp(tx.get("transactionTime", 0) / 1000).strftime("%Y-%m-%d %H:%M:%S")
    payer = tx.get("payerInfo") or {}
    recv = tx.get("receiverInfo") or {}
    wallets = ",".join(WALLET_NAMES.get(w, str(w)) for w in (tx.get("walletTypes") or [tx.get("walletType")]) if w)
    return (
        f"{ts} | {direction(tx, my_uid)} | {tx.get('orderType'):<10} | "
        f"{tx.get('amount'):>16} {tx.get('currency'):<6} | wallet={wallets:<12} | "
        f"payer={payer.get('name')}({payer.get('type')},uid={payer.get('binanceId')}) -> "
        f"recv={recv.get('name')}(uid={recv.get('binanceId')}) | txId={tx.get('transactionId')} | "
        f"orderId={tx.get('orderId')} | cp={tx.get('counterpartyId')} | note={tx.get('note')!r}"
    )


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--days", type=int, default=30, help="回溯天数 (<=90)，默认 30")
    ap.add_argument("--watch", type=int, metavar="SECONDS", help="轮询模式：每 N 秒拉一次，打印新出现的流水")
    ap.add_argument("--raw", action="store_true", help="同时打印原始 JSON（用于确认字段）")
    args = ap.parse_args()

    load_env_file()
    key = os.environ.get("BINANCE_API_KEY")
    secret = os.environ.get("BINANCE_API_SECRET")
    my_uid = os.environ.get("BINANCE_UID")
    if not key or not secret:
        sys.exit("未找到密钥：请设置环境变量 BINANCE_API_KEY / BINANCE_API_SECRET，"
                 f"或创建密钥文件 {DEFAULT_ENV_FILE}（见脚本头部说明）")

    if args.watch:
        seen = set()
        first = True
        print(f"[watch] 每 {args.watch}s 轮询一次（接口权重 3000/次，UID 限额 180000/分钟，最快约 1 次/秒）")
        while True:
            try:
                txs = fetch_pay_transactions(key, secret, start_ms=int((time.time() - 3600) * 1000))
            except SystemExit as e:
                print(f"[error] {e}")
                time.sleep(args.watch)
                continue
            for tx in sorted(txs, key=lambda t: t.get("transactionTime", 0)):
                tid = tx.get("transactionId")
                if tid in seen:
                    continue
                seen.add(tid)
                if not first:
                    print("[NEW] " + fmt(tx, my_uid))
                    if args.raw:
                        print(json.dumps(tx, ensure_ascii=False, indent=2))
            if first:
                print(f"[watch] 初始基线 {len(seen)} 条（最近 1 小时），等待新流水…")
                first = False
            time.sleep(args.watch)

    days = max(1, min(args.days, 90))
    end_ms = int(time.time() * 1000)
    start_ms = end_ms - days * 86400 * 1000
    txs = fetch_pay_transactions(key, secret, start_ms=start_ms, end_ms=end_ms)
    print(f"最近 {days} 天共 {len(txs)} 条 Pay 流水（单次最多 100 条）")
    for tx in sorted(txs, key=lambda t: t.get("transactionTime", 0)):
        print(fmt(tx, my_uid))
    if args.raw:
        print(json.dumps(txs, ensure_ascii=False, indent=2))
    if not txs:
        print("\n提示：返回为空可能是 (a) 该时间段确实没有 Pay 交易 (b) 账号所在地区不支持 Binance Pay，"
              "建议先用 App 给自己/朋友转 0.1 USDT 做一笔 C2C 后再跑一次。")


if __name__ == "__main__":
    main()
