<?php
/* 签名向量测试：php sdk/php/test_vectors.php （CI 中运行） */
require __DIR__ . '/BPayGate.php';

$vf = json_decode(file_get_contents(__DIR__ . '/../../docs/test_vectors.json'), true);
if (!$vf) {
    fwrite(STDERR, "无法读取向量文件\n");
    exit(1);
}
$fails = 0;
foreach ($vf['cases'] as $i => $c) {
    if ($c['type'] === 'request') {
        $got = BPayGate::signRequest($vf['secret'], $c['timestamp'], $c['nonce'], $c['method'], $c['path'], $c['body']);
    } else {
        $got = BPayGate::signCallback($vf['secret'], $c['timestamp'], $c['nonce'], $c['body']);
    }
    if ($got !== $c['signature']) {
        fwrite(STDERR, "case $i ({$c['type']}): got $got want {$c['signature']}\n");
        $fails++;
    }
}
// verifyCallback 正反例
$body = '{"event":"paid","merchant_order_id":"M1"}';
$ts = (string)(int)(microtime(true) * 1000);
$nonce = str_repeat('n', 20);
$sig = BPayGate::signCallback($vf['secret'], $ts, $nonce, $body);
$payload = BPayGate::verifyCallback(
    ['X-Bpg-Timestamp' => $ts, 'X-Bpg-Nonce' => $nonce, 'X-Bpg-Signature' => $sig],
    $body, $vf['secret']
);
if (($payload['event'] ?? '') !== 'paid') {
    fwrite(STDERR, "verifyCallback 解析失败\n");
    $fails++;
}
try {
    BPayGate::verifyCallback(
        ['X-Bpg-Timestamp' => $ts, 'X-Bpg-Nonce' => $nonce, 'X-Bpg-Signature' => 'bad'],
        $body, $vf['secret']
    );
    fwrite(STDERR, "坏签名未被拒绝\n");
    $fails++;
} catch (BPayGateException $e) {
    // 预期
}
if ($fails > 0) {
    exit(1);
}
echo "PHP SDK vectors: ALL PASS\n";
