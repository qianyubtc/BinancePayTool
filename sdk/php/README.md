# BinancePayTool PHP SDK

单文件 `BPayGate.php`，PHP 7.4+，需 curl 扩展。

```php
require 'BPayGate.php';
$gw = new BPayGate('https://pay.example.com', '你的API_AUTH_KEY');
$order = $gw->createOrder([
    'merchant_order_id' => 'SHOP-1001',
    'currency' => 'USDT',
    'amount'   => '10',
    'callback_url' => 'https://shop.example.com/bpg_notify.php',
]);
header('Location: ' . $order['pay_url']);
```

回调：`BPayGate::verifyCallback(getallheaders(), file_get_contents('php://input'), $secret)`，
通过返回数组、失败抛异常；按 `merchant_order_id` 幂等发货后回 200。
完整字段见 [docs/protocol.md](../../docs/protocol.md)。示例：[examples/php](../../examples/php)。
