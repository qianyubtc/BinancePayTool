<?php
/**
 * BPayGate PHP SDK（单文件，PHP 7.4+，需 curl 与 json 扩展）
 *
 * 用法：
 *   require 'BPayGate.php';
 *   $gw = new BPayGate('https://pay.example.com', '你的API_AUTH_KEY');
 *   $order = $gw->createOrder([
 *       'merchant_order_id' => 'SHOP-1001',
 *       'currency' => 'USDT',
 *       'amount' => '10',
 *       'callback_url' => 'https://shop.example.com/bpg_notify.php',
 *       'return_url' => 'https://shop.example.com/orders/1001',
 *   ]);
 *   header('Location: ' . $order['pay_url']);
 *
 * 回调处理（bpg_notify.php）：
 *   $payload = BPayGate::verifyCallback(getallheaders(), file_get_contents('php://input'), $secret);
 *   if ($payload['event'] === 'paid') { ... }   // 按 merchant_order_id 幂等发货
 *   http_response_code(200);
 */

class BPayGateException extends \Exception
{
    public $errCode;
    public $httpStatus;

    public function __construct(string $errCode, string $message = '', int $httpStatus = 0)
    {
        parent::__construct($errCode . ': ' . $message);
        $this->errCode = $errCode;
        $this->httpStatus = $httpStatus;
    }
}

class BPayGate
{
    private $baseUrl;
    private $secret;
    private $timeout;

    public function __construct(string $baseUrl, string $secret, int $timeout = 10)
    {
        $this->baseUrl = rtrim($baseUrl, '/');
        $this->secret = $secret;
        $this->timeout = $timeout;
    }

    /* ---------- API ---------- */

    public function createOrder(array $params): array
    {
        return $this->request('POST', '/api/v1/orders', $params);
    }

    public function getOrder(string $orderId): array
    {
        return $this->request('GET', '/api/v1/orders/' . $orderId);
    }

    public function getOrderByMerchantId(string $merchantOrderId): array
    {
        return $this->request('GET', '/api/v1/orders/by-merchant/' . $merchantOrderId);
    }

    public function closeOrder(string $orderId): array
    {
        return $this->request('POST', '/api/v1/orders/' . $orderId . '/close', []);
    }

    /* ---------- 签名 ---------- */

    public static function signRequest(string $secret, string $ts, string $nonce, string $method, string $path, string $body): string
    {
        $sts = $ts . "\n" . $nonce . "\n" . $method . "\n" . $path . "\n" . hash('sha256', $body);
        return hash_hmac('sha256', $sts, $secret);
    }

    public static function signCallback(string $secret, string $ts, string $nonce, string $body): string
    {
        $sts = $ts . "\n" . $nonce . "\n" . hash('sha256', $body);
        return hash_hmac('sha256', $sts, $secret);
    }

    /**
     * 校验网关回调，成功返回回调数组，失败抛 BPayGateException。
     * $headers 传 getallheaders() 即可（键大小写不敏感）。
     * 注意：回调可能重复投递，商户须按 merchant_order_id 幂等处理。
     */
    public static function verifyCallback(array $headers, string $body, string $secret, int $maxSkewMs = 300000): array
    {
        $h = [];
        foreach ($headers as $k => $v) {
            $h[strtolower($k)] = $v;
        }
        $ts = $h['x-bpg-timestamp'] ?? '';
        $nonce = $h['x-bpg-nonce'] ?? '';
        $sig = $h['x-bpg-signature'] ?? '';
        if ($ts === '' || $nonce === '' || $sig === '') {
            throw new BPayGateException('ERR_AUTH', '缺少签名头');
        }
        if (!ctype_digit($ts) || abs((int)(microtime(true) * 1000) - (int)$ts) > $maxSkewMs) {
            throw new BPayGateException('ERR_AUTH', '时间戳非法或偏差过大');
        }
        if (!hash_equals(self::signCallback($secret, $ts, $nonce, $body), $sig)) {
            throw new BPayGateException('ERR_AUTH', '签名错误');
        }
        $payload = json_decode($body, true);
        if (!is_array($payload)) {
            throw new BPayGateException('ERR_PARAM', '回调体解析失败');
        }
        return $payload;
    }

    /* ---------- 内部 ---------- */

    private function request(string $method, string $path, ?array $payload = null): array
    {
        $body = $payload === null ? '' : json_encode($payload, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES);
        $ts = (string)(int)(microtime(true) * 1000);
        $nonce = bin2hex(random_bytes(12));
        $sig = self::signRequest($this->secret, $ts, $nonce, $method, $path, $body);

        $ch = curl_init($this->baseUrl . $path);
        curl_setopt_array($ch, [
            CURLOPT_CUSTOMREQUEST => $method,
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_TIMEOUT => $this->timeout,
            CURLOPT_HTTPHEADER => [
                'Content-Type: application/json',
                'X-BPG-Timestamp: ' . $ts,
                'X-BPG-Nonce: ' . $nonce,
                'X-BPG-Signature: ' . $sig,
            ],
        ]);
        if ($body !== '') {
            curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
        }
        $raw = curl_exec($ch);
        if ($raw === false) {
            $err = curl_error($ch);
            curl_close($ch);
            throw new BPayGateException('ERR_NETWORK', $err);
        }
        $status = (int)curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
        curl_close($ch);

        $out = json_decode($raw, true);
        if (!is_array($out)) {
            throw new BPayGateException('ERR_HTTP', 'HTTP ' . $status, $status);
        }
        if (($out['code'] ?? '') !== 'OK') {
            throw new BPayGateException($out['code'] ?? 'ERR_UNKNOWN', $out['message'] ?? '', $status);
        }
        return $out['data'];
    }
}
