package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "./config.env", "配置文件路径（KEY=VALUE 格式，环境变量优先）")
	showVer := flag.Bool("version", false, "打印版本")
	genKey := flag.Bool("gen-key", false, "生成一个随机 API_AUTH_KEY")
	flag.Parse()

	if *showVer {
		fmt.Println("BinancePayTool", version)
		return
	}
	if *genKey {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			log.Fatal(err)
		}
		fmt.Println(base64.RawURLEncoding.EncodeToString(b))
		return
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("[fatal] %v", err)
	}
	app, err := newApp(cfg)
	if err != nil {
		log.Fatalf("[fatal] 初始化失败: %v", err)
	}
	defer app.st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.matcher.run(ctx)
	go app.notifier.run(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: app.mux}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	log.Printf("[info] BinancePayTool %s 监听 %s，收款 UID %s，轮询间隔 %ds", version, cfg.Listen, cfg.BinanceUID, cfg.PollInterval)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[fatal] %v", err)
	}
	log.Printf("[info] 已退出")
}
