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

// version 是本套件的版本号，也是唯一的版本号。
//
// 采用纯 x.y.z，**不再带 -000N 构建号**：套件 INFO 的 version、catalog 的
// version、控制台显示的「当前版本 / 最新版本」、GitHub Release 的 tag 全部
// 来自这一个值，发新版直接 +1（1.0.13 → 1.0.14）。
//
// 之前用「功能号-构建号」（1.0.12-0002）是为了能在功能号不变时重发，
// 但代价是三处显示不一致：套件装的是 1.0.12-0002、控制台当前版本显示 1.0.12、
// Release tag 又是 v1.0.12-0002，用户根本对不上。而且 updater 的版本比较会在
// 第一个 '-' 处截断，1.0.12-0002 与 1.0.12 被判成相等，永远显示「已是最新」。
// 统一成一个号之后这些问题自然消失。
var version = "1.0.17"

// releaseRepo 是控制台「版本与自更新」卡片里「查看全部版本」要跳转的 GitHub 仓库
// （owner/name），非套件版会被自更新仓库覆盖，见 main()。这里是本 DSM 套件分支的仓库：
// 套件版禁用了自更新、升级由群晖套件中心负责，所以链接必须指向套件自己的 Release，
// 不能指向上游 —— 上游的 Release 里没有 SPK，用户点进去会一脸懵。
// 换 fork / 换发布仓库时同步改这一行。
var releaseRepo = "kosje/agnes-hub-go-DSM"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	// 自更新助手模式：此时本进程唯一任务是等上一个进程退出、
	// 换掉被锁定的 exe、再把新版本拉起来，然后立刻结束。
	// 必须在任何初始化之前判断，否则助手会去抢端口。
	if updater.SwapHelperRequested() {
		os.Exit(updater.RunSwapHelper())
	}

	host := flag.String("host", env("AGNES_HUB_HOST", "127.0.0.1"), "监听地址（0.0.0.0 表示允许局域网访问）")
	port := flag.String("port", env("AGNES_HUB_PORT", "4142"), "监听端口")
	dataDir := flag.String("data", env("AGNES_HUB_DATA", ""), "数据目录（默认 ./data）")
	noSelfUpdate := flag.Bool("no-selfupdate", env("AGNES_HUB_NO_SELFUPDATE", "") != "",
		"禁用内置自更新（由套件中心 / 系统包管理器负责升级时使用）")
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
	srv.Version = version

	// 初始化自更新器
	exe, _ := os.Executable()
	// 检查频率：12 小时。自更新是「有就换」，没必要更勤。
	updRepo, checkInterval := "my788525/agnes-hub-go", 12*time.Hour
	// 控制台「查看全部版本」的跳转目标：默认用套件仓库，自更新开着时跟随自更新仓库，
	// 否则用户点进去看到的版本跟「立即更新」能装上的版本会对不上。
	relRepo := releaseRepo
	if *noSelfUpdate {
		// 群晖套件等场景：二进制由套件中心管理，套件 INFO 里登记了 package.tgz 的 checksum。
		// 若允许进程内替换二进制，套件的「实际内容」与「已安装版本」就会不一致，
		// 下次套件中心校验或升级必然冲突 —— 所以应用更新必须关掉。
		//
		// 但「查最新版本号」是只读的、没有任何副作用，必须保留：控制台要如实显示
		// 「最新版本 / 发布时间 / 是否已是最新」，否则这三格永远是一片「—」，
		// 用户根本不知道有没有新版。所以这里仍然配 releaseRepo，只是标记为套件托管。
		updRepo = releaseRepo
	}
	updCfg := updater.Config{
		Repo:          updRepo,
		BinaryName:    "agnes-hub-go",
		DataDir:       *dataDir,
		CheckInterval: checkInterval,
		// 套件版的 release 资产是 SPK（agnes-hub-x86_64-1.0.13.spk），
		// 不是裸二进制，pickAsset 永远挑不到 —— 若不告诉 updater，它会判定
		// 「没有本平台的资产」→ IsUpdateAvailable 恒为 false，控制台永远显示
		// 「已是最新」，还会弹一条莫名其妙的资产缺失告警。
		SuiteManaged: *noSelfUpdate,
	}
	upd := updater.New(updCfg, version, exe, nil)
	srv.SetUpdater(upd)
	srv.SetSuiteManaged(*noSelfUpdate)
	srv.SetReleaseRepo(relRepo)
	upd.StartBackground(ctx, checkInterval)

	// 启动后立刻查一次：StartBackground 要等一个周期（12h）才首次检查，
	// 控制台的「最新版本 / 发布时间」会空很久。
	go func() {
		if _, err := upd.Check(ctx); err != nil {
			log.Printf("检查更新失败：%v", err)
		}
	}()

	// 应用完更新后要真的退出：光置一个标志位没人看，
	// 必须有人把它翻译成取消信号，进程才会走到优雅关闭。
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if upd.NeedRestart() {
					fmt.Println("自更新已就位，正在退出以便替换二进制…")
					cancel()
					return
				}
			}
		}
	}()

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
		fmt.Println("  请使用安装向导设置的管理员密码登录控制台；若未设置，可通过重新安装向导填写新密码覆盖。")
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

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 8*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
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
		wg.Wait()
	case e := <-errCh:
		log.Printf("监听错误：%v", e)
		cancel()
		wg.Wait()
	}
	fmt.Println("Agnes Hub 已停止。")
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
