import com.bpaygate.sdk.BPayGateClient;
import com.google.gson.JsonObject;
import com.sun.net.httpserver.HttpServer;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;

/**
 * 最小商户示例（Java 8+，无 Web 框架）。
 * 编译运行后访问 http://127.0.0.1:9000/buy
 */
public class ExampleServer {
    public static void main(String[] args) throws Exception {
        String base = System.getenv().getOrDefault("BPG_BASE", "http://127.0.0.1:8080");
        String secret = System.getenv().getOrDefault("BPG_SECRET", "");
        BPayGateClient gw = new BPayGateClient(base, secret);

        HttpServer srv = HttpServer.create(new InetSocketAddress("127.0.0.1", 9000), 0);
        srv.createContext("/buy", ex -> {
            JsonObject order = gw.createOrder("DEMO-" + System.currentTimeMillis(), "USDT", "1",
                    "http://127.0.0.1:9000/notify", "http://127.0.0.1:9000/done", 0);
            ex.getResponseHeaders().add("Location", order.get("pay_url").getAsString());
            ex.sendResponseHeaders(302, -1);
            ex.close();
        });
        srv.createContext("/notify", ex -> {
            String body = readAll(ex.getRequestBody());
            Map<String, String> headers = new HashMap<>();
            ex.getRequestHeaders().forEach((k, v) -> headers.put(k, v.isEmpty() ? "" : v.get(0)));
            try {
                JsonObject payload = BPayGateClient.verifyCallback(headers, body, secret, 300000);
                // TODO: 按 merchant_order_id 幂等发货
                System.out.println("回调: " + payload.get("event") + " " + payload.get("merchant_order_id"));
                ex.sendResponseHeaders(200, -1);
            } catch (BPayGateClient.BPayGateException e) {
                ex.sendResponseHeaders(401, -1);
            }
            ex.close();
        });
        srv.createContext("/done", ex -> {
            byte[] b = "<h1>支付完成，感谢购买！</h1>".getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().add("Content-Type", "text/html; charset=utf-8");
            ex.sendResponseHeaders(200, b.length);
            try (OutputStream os = ex.getResponseBody()) { os.write(b); }
        });
        System.out.println("商户示例: http://127.0.0.1:9000/buy");
        srv.start();
    }

    private static String readAll(InputStream is) throws java.io.IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[4096];
        int n;
        while ((n = is.read(buf)) > 0) bos.write(buf, 0, n);
        return new String(bos.toByteArray(), StandardCharsets.UTF_8);
    }
}
