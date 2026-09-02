# BinancePayTool Java SDK

Java 8+，依赖 Gson（见 pom.xml）。也可直接把 `BPayGateClient.java` 拷进项目。

```java
BPayGateClient gw = new BPayGateClient("https://pay.example.com", "你的API_AUTH_KEY");
JsonObject order = gw.createOrder("SHOP-1001", "USDT", "10",
        "https://shop.example.com/bpg/notify", null, 0);
String payUrl = order.get("pay_url").getAsString();

// 回调:
JsonObject payload = BPayGateClient.verifyCallback(headersMap, rawBody, SECRET, 300000);
```

完整字段见 [docs/protocol.md](../../docs/protocol.md)。示例：[examples/java](../../examples/java)。
