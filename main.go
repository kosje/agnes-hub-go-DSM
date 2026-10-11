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
	"sync"
	"syscall"
	"time"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/relay"
	"agneshub/internal/updater"
	"agneshub/internal/web"
)

var version = "1.0.35"

// releaseRepo 是本项目的发布仓库（owner/name）：控制台「查看全部版本」跳转到这里，
// 「最新版本」也从这里查。不能指向上游 —— 上游的 Release 里没有 SPK，版本号也对不上。
// 换 fork / 换发布仓库时同步改这一行。
var releaseRepo = "kosje/agnes-hub-go-DSM"

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
	// 进程内自更新已整体关闭（见下方「初始化更新检查」），这个开关现在只用来标记
	// 「由套件中心管理」，决定控制台给出哪种升级指引。群晖 SPK 启动时自动带上。
	noSelfUpdate := flag.Bool("no-selfupdate", env("AGNES_HUB_NO_SELFUPDATE", "") != "",
		"标记由套件中心 / 系统包管理器负责升级（群晖套件使用）")
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
	maintenanceDone := h.StartMaintenance(ctx)
	h.StartHistory(ctx)

	srv := web.New(store, h, relay.BuildClientForAccounts(len(store.AccountsSnapshot())))
	srv.Version = version

	// 初始化更新检查：只查、不装。
	//
	// 进程内自更新（下载新二进制替换自身）在所有平台上都关闭：
	//   - 群晖：二进制由套件中心管理，INFO 里登记了 package.tgz 的 checksum，
	//     进程内替换会让「实际内容」与「已安装版本」对不上；
	//   - Windows：替换助手是从正要被覆盖的那个 exe 本身启动的，运行中的映像被
	//     自己锁住，替换注定失败，而旧进程已经退出 —— 每次「更新」都会让服务停掉；
	//     且旧配置指向第三方仓库、下载后不校验摘要。
	// 升级改为：群晖走套件中心，Windows 到本仓库 Release 下载后手动替换。
	//
	// 「查最新版本号」是只读的、没有副作用，保留：控制台要如实显示
	// 「最新版本 / 发布时间 / 是否已是最新」。检查频率 12 小时。
	checkInterval := 12 * time.Hour
	upd := updater.New(updater.Config{Repo: releaseRepo}, version, nil)
	srv.SetUpdater(upd)
	srv.SetSuiteManaged(true)
	srv.SetPackageManaged(*noSelfUpdate)
	srv.SetReleaseRepo(releaseRepo)
	upd.StartBackground(ctx, checkInterval)

	// 启动后立刻查一次：StartBackground 要等一个周期（12h）才首次检查，
	// 控制台的「最新版本 / 发布时间」会空很久。
	go func() {
		if _, err := upd.Check(ctx); err != nil {
			log.Printf("检查更新失败：%v", err)
		}
	}()

	// 数据保留维护：按设置里的保留天数清理图片记录与用量日志，并清掉被中断的下载临时文件。
	// 启动一分钟后跑第一次（不和启动抢 I/O），之后每 6 小时一次。
	go func() {
		runJanitor := func() {
			imgs, logs := store.PruneRetention(time.Now())
			parts := srv.CleanStaleMediaParts(6 * time.Hour)
			if imgs+logs+parts > 0 {
				log.Printf("保留期清理：图片记录 %d 条、用量日志 %d 行、残留下载文件 %d 个", imgs, logs, parts)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		runJanitor()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runJanitor()
			}
		}
	}()

	addr := net.JoinHostPort(*host, *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 20 * time.Second,
		// ReadTimeout 覆盖「读取整个请求体」的时间上限。此前未设置，意味着：
		// 客户端一旦没能把自己声明的请求体发完（连接假死 / 超大上下文传输中断 /
		// 中间代理掐断），handler 会一直阻塞在 io.ReadAll(r.Body) 上——
		// goroutine 永久泄漏，客户端那边一直转圈，而网关的所有指标都显示「什么都没发生」
		// （requests_total 不涨、用量日志不写、控制台负载为 0），排障时极易误判成
		// 「请求根本没到网关」。这里给一个远大于正常上传耗时的上限兜底。
		// 注意它只约束【请求体的读取】，不影响流式 SSE 的响应写出时长。
		ReadTimeout: 5 * time.Minute,
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
	fmt.Printf("  Agens-Hub %s  已启动\n", version)
	fmt.Println("--------------------------------------------------------------")
	fmt.Printf("  控制台    http://%s/console\n", displayAddr(*host, *port))
	fmt.Printf("  接口基址  http://%s/v1\n", displayAddr(*host, *port))
	fmt.Printf("  统一模型  %s（自动判定 文本/生图/生视频）\n", settings.AutoModelName)
	fmt.Printf("  数据目录  %s\n", *dataDir)
	fmt.Printf("  可用账号  %d\n", enabled)
	if settings.MustChangePassword {
		fmt.Println("  管理员仍是初始密码 admin123：登录控制台后必须先改密，改之前其它管理功能不可用。")
	}
	if *host == "0.0.0.0" || *host == "" || *host == "::" {
		fmt.Println("  网络监听  IPv4(0.0.0.0) + IPv6(::) 双栈")
	}
	fmt.Println("--------------------------------------------------------------")
	fmt.Println("  关闭本窗口即停止服务。")
	fmt.Println("==============================================================")

	listeners, err := buildListeners(*host, *port)
	if err != nil {
		log.Fatalf("监听失败：%v", err)
	}

	// Serve 在 Shutdown 一开始就返回，所以 main 必须另外等 Shutdown 本身结束，
	// 在途请求才有 8 秒收尾，维护循环最后一次统计落盘也不会被进程退出抢先。
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		// Stop accepting first. Keep enough time to save state before the DSM
		// lifecycle script's 15-second forced-stop deadline.
		shutdownCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
		defer c()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("请求收尾超时，取消剩余连接：%v", err)
			_ = httpSrv.Close()
		}
		srv.WaitHandlers()
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, len(listeners))
	for _, ln := range listeners {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			if e := httpSrv.Serve(l); e != nil && !errors.Is(e, http.ErrServerClosed) {
				errCh <- e
			}
		}(ln)
	}

	// 等到优雅关闭完成，或首个监听错误。
	select {
	case <-ctx.Done():
	case e := <-errCh:
		log.Printf("监听错误：%v", e)
		cancel()
	}
	wg.Wait()
	<-shutdownDone
	<-maintenanceDone
	// Flush again after in-flight requests finish.
	if err := store.FlushPending(); err != nil {
		log.Printf("停止服务时保存数据失败：%v", err)
	}
	fmt.Println("Agens-Hub 已停止。")
}

// buildListeners 按平台拆分到 listener_linux.go / listener_other.go：
//   - Linux（群晖套件的部署目标）：host 为 0.0.0.0/空/:: 时同时监听
//     IPv4(0.0.0.0) 与 IPv6(::)，IPv6 套接字强制 V6ONLY=1，互不抢占端口，
//     满足外网 IPv6 域名直达 + 局域网 IPv4 访问。
//   - 其余平台（Windows / macOS 单机运行）：单套接字，行为与原版一致。

func displayAddr(host, port string) string {
	switch host {
	case "0.0.0.0", "::", "":
		return "127.0.0.1:" + port
	default:
		return net.JoinHostPort(host, port)
	}
}
