<?php
/* 最小商户示例：创建订单并跳转收银页。
   php -S 127.0.0.1:9000 后访问 http://127.0.0.1:9000/index.php */
require __DIR__ . '/../../sdk/php/BPayGate.php';

$gw = new BPayGate(getenv('BPG_BASE') ?: 'http://127.0.0.1:8080', getenv('BPG_SECRET') ?: '');
$order = $gw->createOrder([
    'merchant_order_id' => 'DEMO-' . time(),
    'currency' => 'USDT',
    'amount' => '1',
    'callback_url' => 'https://你的域名/notify.php',
    'return_url' => 'https://你的域名/done.php',
]);
header('Location: ' . $order['pay_url']);
