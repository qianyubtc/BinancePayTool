# BinancePayTool Python SDK

仅标准库，Python 3.7+。把 `bpaygate.py` 拷进项目或按路径引入即可。

```python
from bpaygate import BPayGate, BPayGateError

gw = BPayGate("https://pay.example.com", "你的API_AUTH_KEY")
order = gw.create_order("SHOP-1001", "10", currency="USDT",
                        callback_url="https://shop.example.com/bpg/notify")
# 跳转用户到 order["pay_url"]

# 回调（Flask 写法）:
payload = BPayGate.verify_callback(request.headers, request.get_data(), SECRET)
if payload["event"] == "paid":
    ...  # 按 merchant_order_id 幂等发货
```

方法：`create_order` / `get_order` / `get_order_by_merchant_id` / `close_order` / `verify_callback`。
完整字段见 [docs/protocol.md](../../docs/protocol.md)。示例：[examples/python](../../examples/python)。
