// 最小商户示例：go run . 后访问 http://127.0.0.1:9000/buy
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	bpaygate "github.com/qianyubtc/BinancePayTool/sdk/go"
)

func main() {
	base := envOr("BPG_BASE", "http://127.0.0.1:8080")
	secret := os.Getenv("BPG_SECRET")
	gw := bpaygate.New(base, secret)

	http.HandleFunc("/buy", func(w http.ResponseWriter, r *http.Request) {
		order, err := gw.CreateOrder(bpaygate.CreateOrderReq{
			MerchantOrderID: fmt.Sprintf("DEMO-%d", time.Now().Unix()),
			Currency:        "USDT",
			Amount:          "1",
			CallbackURL:     "http://127.0.0.1:9000/notify",
			ReturnURL:       "http://127.0.0.1:9000/done",
		})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		http.Redirect(w, r, order.PayURL, http.StatusFound)
	})
	http.HandleFunc("/notify", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		cb, err := bpaygate.VerifyCallback(r.Header, body, secret, 5*time.Minute)
		if err != nil {
			http.Error(w, "bad signature", 401)
			return
		}
		// TODO: 按 cb.MerchantOrderID 幂等发货
		log.Printf("回调: %s %s %s%s", cb.Event, cb.MerchantOrderID, cb.ActualAmount, cb.Currency)
		w.WriteHeader(200)
	})
	http.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<h1>支付完成，感谢购买！</h1>")
	})
	log.Println("商户示例: http://127.0.0.1:9000/buy")
	log.Fatal(http.ListenAndServe("127.0.0.1:9000", nil))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
