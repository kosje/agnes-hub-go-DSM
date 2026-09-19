// agnes-hub-go — Agnes AI 多账号聚合中转 + RPM 限流排队网关
//
// 一句话说明它解决什么：把「20 RPM 硬墙 + 弹超限提示 + 任务中断」
// 变成「服务端自己排队等候，客户端永远拿不到 429，任务不断线」，
// 并把多个独立账号聚合成线性可扩容的池，按模态分工承载文本 / 生图 / 生视频。
//
// 客户端只需要填一个模型名 agnes-auto，其余交给网关。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/relay"
	"agneshub/internal/web"
)

var version = "1.0.0"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	host := flag.String("host", env("AGNES_HUB_HOST", "127.0.0.1"), "监听地址（0.0.0.0 表示允许局域网访问）")
	port := flag.String("port", env("AGNES_HUB_PORT", "4142"), "监听端口")
	dataDir := flag.String("data", env("AGNES_HUB_DATA", ""), "数据目录（默认 ./data）")
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println("agnes-hub-go", version)
		return
	}

	if *dataDir == "" {
		exe, err := os.Executable()
		if err == nil {
			*dataDir = filepath.Join(filepath.Dir(exe), "data")
		} else {
			*dataDir = "data"
		}
	}
	if abs, err := filepath.Abs(*dataDir); err == nil {
		*dataDir = abs
	}

	store, err := config.NewStore(*dataDir)
	if err != nil {
		log.Fatalf("初始化数据目录失败：%v", err)
	}
	h := hub.New(store)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	h.StartMaintenance(ctx)

	srv := web.New(store, h, relay.BuildClient())

	addr := net.JoinHostPort(*host, *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 20 * time.Second,
		// 刻意不设 WriteTimeout / IdleTimeout 上限：
		// 视频提交与长回答的 SSE 流可能持续很久，超时会把正常任务掐断。
		IdleTimeout: 120 * time.Second,
	}

	settings := store.SettingsSnapshot()
	enabled := 0
	for _, a := range store.AccountsSnapshot() {
		if a.Enabled && a.APIKey != "" {
			enabled++
		}
	}

	fmt.Println("==============================================================")
	fmt.Printf("  agnes-hub-go %s  已启动\n", version)
	fmt.Println("--------------------------------------------------------------")
	fmt.Printf("  控制台    http://%s/console\n", displayAddr(*host, *port))
	fmt.Printf("  接口基址  http://%s/v1\n", displayAddr(*host, *port))
	fmt.Printf("  统一模型  %s（自动判定 文本/生图/生视频）\n", settings.AutoModelName)
	fmt.Printf("  数据目录  %s\n", *dataDir)
	fmt.Printf("  可用账号  %d\n", enabled)
	if settings.MustChangePassword {
		fmt.Println("  初始密码  admin123   ← 请到控制台立即修改")
	}
	fmt.Println("--------------------------------------------------------------")
	fmt.Println("  关闭本窗口即停止服务。")
	fmt.Println("==============================================================")

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("监听失败：%v", err)
	}
	fmt.Println("agnes-hub-go 已停止。")
}

func displayAddr(host, port string) string {
	switch host {
	case "0.0.0.0", "::", "":
		return "127.0.0.1:" + port
	default:
		return net.JoinHostPort(host, port)
	}
}
