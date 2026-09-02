# BinancePayTool Go SDK

仅标准库。模块路径 `github.com/qianyubtc/BinancePayTool/sdk/go`；仓库发布前本地可用 `replace` 指到目录（见 examples/go/go.mod）。

```go
gw := bpaygate.New("https://pay.example.com", "你的API_AUTH_KEY")
order, err := gw.CreateOrder(bpaygate.CreateOrderReq{
    MerchantOrderID: "SHOP-1001", Currency: "USDT", Amount: "10",
    CallbackURL: "https://shop.example.com/bpg/notify",
})
// 跳转用户到 order.PayURL

// 回调:
cb, err := bpaygate.VerifyCallback(r.Header, body, secret, 5*time.Minute)
```

完整字段见 [docs/protocol.md](../../docs/protocol.md)。示例：[examples/go](../../examples/go)。
