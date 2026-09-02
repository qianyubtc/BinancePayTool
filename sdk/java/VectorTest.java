import com.bpaygate.sdk.BPayGateClient;
import com.google.gson.Gson;
import com.google.gson.JsonArray;
import com.google.gson.JsonObject;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Paths;

/** 签名向量测试（无 JUnit 依赖，CI 中直接 java 运行；非零退出即失败）。 */
public class VectorTest {
    public static void main(String[] args) throws Exception {
        String path = args.length > 0 ? args[0] : "../../docs/test_vectors.json";
        String raw = new String(Files.readAllBytes(Paths.get(path)), StandardCharsets.UTF_8);
        JsonObject vf = new Gson().fromJson(raw, JsonObject.class);
        String secret = vf.get("secret").getAsString();
        JsonArray cases = vf.getAsJsonArray("cases");
        int fails = 0;
        for (int i = 0; i < cases.size(); i++) {
            JsonObject c = cases.get(i).getAsJsonObject();
            String got;
            if ("request".equals(c.get("type").getAsString())) {
                got = BPayGateClient.signRequest(secret, c.get("timestamp").getAsString(),
                        c.get("nonce").getAsString(), c.get("method").getAsString(),
                        c.get("path").getAsString(), c.get("body").getAsString());
            } else {
                got = BPayGateClient.signCallback(secret, c.get("timestamp").getAsString(),
                        c.get("nonce").getAsString(), c.get("body").getAsString());
            }
            if (!got.equals(c.get("signature").getAsString())) {
                System.err.println("case " + i + ": got " + got + " want " + c.get("signature").getAsString());
                fails++;
            }
        }
        // verifyCallback 正反例
        String body = "{\"event\":\"paid\",\"merchant_order_id\":\"M1\"}";
        String ts = String.valueOf(System.currentTimeMillis());
        String sig = BPayGateClient.signCallback(secret, ts, "nnnnnnnnnnnnnnnnnnnn", body);
        JsonObject payload = BPayGateClient.verifyCallback(ts, "nnnnnnnnnnnnnnnnnnnn", sig, body, secret, 300000);
        if (!"paid".equals(payload.get("event").getAsString())) {
            System.err.println("verifyCallback 解析失败");
            fails++;
        }
        try {
            BPayGateClient.verifyCallback(ts, "nnnnnnnnnnnnnnnnnnnn", "bad", body, secret, 300000);
            System.err.println("坏签名未被拒绝");
            fails++;
        } catch (BPayGateClient.BPayGateException e) {
            // 预期
        }
        if (fails > 0) System.exit(1);
        System.out.println("Java SDK vectors: ALL PASS");
    }
}
