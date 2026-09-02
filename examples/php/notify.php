<?php
/* 回调处理示例 */
require __DIR__ . '/../../sdk/php/BPayGate.php';

try {
    $payload = BPayGate::verifyCallback(getallheaders(), file_get_contents('php://input'), getenv('BPG_SECRET') ?: '');
} catch (BPayGateException $e) {
    http_response_code(401);
    exit;
}
if ($payload['event'] === 'paid') {
    // TODO: 按 merchant_order_id 幂等发货
    error_log('已支付: ' . $payload['merchant_order_id'] . ' ' . $payload['actual_amount'] . $payload['currency']);
}
http_response_code(200);
