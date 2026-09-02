package com.bpaygate.sdk;

import com.google.gson.Gson;
import com.google.gson.JsonObject;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.util.Map;

/**
 * BPayGate Java SDK（Java 8+，依赖 Gson）。
 *
 * <pre>
 * BPayGateClient gw = new BPayGateClient("https://pay.example.com", "你的API_AUTH_KEY");
 * JsonObject order = gw.createOrder("SHOP-1001", "USDT", "10",
 *         "https://shop.example.com/bpg/notify", "https://shop.example.com/orders/1001", 0);
 * String payUrl = order.get("pay_url").getAsString();  // 跳转用户
 *
 * // 回调处理：
 * JsonObject payload = BPayGateClient.verifyCallback(tsHeader, nonceHeader, sigHeader, rawBody, SECRET, 300000);
 * if ("paid".equals(payload.get("event").getAsString())) { ... } // 按 merchant_order_id 幂等发货
 * </pre>
 */
public class BPayGateClient {

    public static class BPayGateException extends RuntimeException {
        public final String code;
        public final int httpStatus;

        public BPayGateException(String code, String message, int httpStatus) {
            super(code + ": " + message);
            this.code = code;
            this.httpStatus = httpStatus;
        }
    }

    private static final Gson GSON = new Gson();
    private static final SecureRandom RND = new SecureRandom();

    private final String baseUrl;
    private final String secret;
    private final int timeoutMs;

    public BPayGateClient(String baseUrl, String secret) {
        this(baseUrl, secret, 10000);
    }

    public BPayGateClient(String baseUrl, String secret, int timeoutMs) {
        String b = baseUrl;
        while (b.endsWith("/")) b = b.substring(0, b.length() - 1);
        this.baseUrl = b;
        this.secret = secret;
        this.timeoutMs = timeoutMs;
    }

    /* ---------- API ---------- */

    public JsonObject createOrder(String merchantOrderId, String currency, String amount,
                                  String callbackUrl, String returnUrl, long timeoutSec) {
        JsonObject p = new JsonObject();
        p.addProperty("merchant_order_id", merchantOrderId);
        p.addProperty("currency", currency);
        p.addProperty("amount", amount);
        if (callbackUrl != null && !callbackUrl.isEmpty()) p.addProperty("callback_url", callbackUrl);
        if (returnUrl != null && !returnUrl.isEmpty()) p.addProperty("return_url", returnUrl);
        if (timeoutSec > 0) p.addProperty("timeout", timeoutSec);
        return request("POST", "/api/v1/orders", GSON.toJson(p));
    }

    public JsonObject getOrder(String orderId) {
        return request("GET", "/api/v1/orders/" + orderId, "");
    }

    public JsonObject getOrderByMerchantId(String merchantOrderId) {
        return request("GET", "/api/v1/orders/by-merchant/" + merchantOrderId, "");
    }

    public JsonObject closeOrder(String orderId) {
        return request("POST", "/api/v1/orders/" + orderId + "/close", "{}");
    }

    /* ---------- 签名 ---------- */

    public static String signRequest(String secret, String ts, String nonce, String method, String path, String body) {
        String sts = ts + "\n" + nonce + "\n" + method + "\n" + path + "\n" + sha256Hex(body.getBytes(StandardCharsets.UTF_8));
        return hmacHex(secret, sts);
    }

    public static String signCallback(String secret, String ts, String nonce, String body) {
        String sts = ts + "\n" + nonce + "\n" + sha256Hex(body.getBytes(StandardCharsets.UTF_8));
        return hmacHex(secret, sts);
    }

    /**
     * 校验网关回调，通过返回回调 JSON，失败抛 BPayGateException。
     * 注意：回调可能重复投递，商户须按 merchant_order_id 幂等处理。
     */
    public static JsonObject verifyCallback(String ts, String nonce, String sig, String body,
                                            String secret, long maxSkewMs) {
        if (ts == null || nonce == null || sig == null || ts.isEmpty() || nonce.isEmpty() || sig.isEmpty()) {
            throw new BPayGateException("ERR_AUTH", "缺少签名头", 0);
        }
        long tsv;
        try {
            tsv = Long.parseLong(ts);
        } catch (NumberFormatException e) {
            throw new BPayGateException("ERR_AUTH", "时间戳非法", 0);
        }
        if (Math.abs(System.currentTimeMillis() - tsv) > maxSkewMs) {
            throw new BPayGateException("ERR_AUTH", "时间戳偏差过大", 0);
        }
        if (!constantTimeEquals(signCallback(secret, ts, nonce, body), sig)) {
            throw new BPayGateException("ERR_AUTH", "签名错误", 0);
        }
        return GSON.fromJson(body, JsonObject.class);
    }

    /** 便捷重载：直接传 header map（键不区分大小写）。 */
    public static JsonObject verifyCallback(Map<String, String> headers, String body, String secret, long maxSkewMs) {
        String ts = null, nonce = null, sig = null;
        for (Map.Entry<String, String> e : headers.entrySet()) {
            String k = e.getKey() == null ? "" : e.getKey().toLowerCase();
            if (k.equals("x-bpg-timestamp")) ts = e.getValue();
            else if (k.equals("x-bpg-nonce")) nonce = e.getValue();
            else if (k.equals("x-bpg-signature")) sig = e.getValue();
        }
        return verifyCallback(ts, nonce, sig, body, secret, maxSkewMs);
    }

    /* ---------- 内部 ---------- */

    private JsonObject request(String method, String path, String body) {
        try {
            String ts = String.valueOf(System.currentTimeMillis());
            byte[] nb = new byte[16];
            RND.nextBytes(nb);
            String nonce = hex(nb);
            String sig = signRequest(secret, ts, nonce, method, path, body);

            HttpURLConnection conn = (HttpURLConnection) new URL(baseUrl + path).openConnection();
            conn.setRequestMethod(method);
            conn.setConnectTimeout(timeoutMs);
            conn.setReadTimeout(timeoutMs);
            conn.setRequestProperty("Content-Type", "application/json");
            conn.setRequestProperty("X-BPG-Timestamp", ts);
            conn.setRequestProperty("X-BPG-Nonce", nonce);
            conn.setRequestProperty("X-BPG-Signature", sig);
            if (!body.isEmpty()) {
                conn.setDoOutput(true);
                try (OutputStream os = conn.getOutputStream()) {
                    os.write(body.getBytes(StandardCharsets.UTF_8));
                }
            }
            int status = conn.getResponseCode();
            InputStream is = status >= 400 ? conn.getErrorStream() : conn.getInputStream();
            String raw = readAll(is);
            JsonObject out;
            try {
                out = GSON.fromJson(raw, JsonObject.class);
            } catch (Exception e) {
                throw new BPayGateException("ERR_HTTP", "HTTP " + status, status);
            }
            if (out == null || out.get("code") == null) {
                throw new BPayGateException("ERR_HTTP", "HTTP " + status, status);
            }
            String code = out.get("code").getAsString();
            if (!"OK".equals(code)) {
                String msg = out.get("message") == null ? "" : out.get("message").getAsString();
                throw new BPayGateException(code, msg, status);
            }
            return out.getAsJsonObject("data");
        } catch (BPayGateException e) {
            throw e;
        } catch (Exception e) {
            throw new BPayGateException("ERR_NETWORK", e.getMessage() == null ? e.toString() : e.getMessage(), 0);
        }
    }

    private static String readAll(InputStream is) throws Exception {
        if (is == null) return "";
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[4096];
        int n;
        while ((n = is.read(buf)) > 0) bos.write(buf, 0, n);
        return new String(bos.toByteArray(), StandardCharsets.UTF_8);
    }

    private static String sha256Hex(byte[] b) {
        try {
            return hex(MessageDigest.getInstance("SHA-256").digest(b));
        } catch (Exception e) {
            throw new RuntimeException(e);
        }
    }

    private static String hmacHex(String secret, String msg) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            return hex(mac.doFinal(msg.getBytes(StandardCharsets.UTF_8)));
        } catch (Exception e) {
            throw new RuntimeException(e);
        }
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(b.length * 2);
        for (byte x : b) sb.append(String.format("%02x", x));
        return sb.toString();
    }

    private static boolean constantTimeEquals(String a, String b) {
        byte[] x = a.getBytes(StandardCharsets.UTF_8);
        byte[] y = b.getBytes(StandardCharsets.UTF_8);
        if (x.length != y.length) return false;
        int r = 0;
        for (int i = 0; i < x.length; i++) r |= x[i] ^ y[i];
        return r == 0;
    }
}
