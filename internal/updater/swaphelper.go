package updater

import (
	"os"
	"path/filepath"
	"time"
)

// 助手内部参数。用不常见的短横线前缀，避免和正常命令行参数冲突。
const (
	swapHelperFlag    = "--baipiao-hub-swap-helper"
	swapSrcArg        = "--src"
	swapDstArg        = "--dst"
	swapNoRelaunchArg = "--no-relaunch"
	swapRelaunchArg   = "--relaunch-arg"
)

// 这三个是 var 而非 const：测试需要把等待时间压到毫秒级，
// 否则验证「等锁释放」和「超时放弃」两条路径都得真等一分半。
var (
	swapHelperTimeout = 90 * time.Second
	swapPollInterval  = 300 * time.Millisecond
	swapSettleDelay   = 900 * time.Millisecond
)

// SwapHelperRequested 判断当前进程是否是「替换助手」实例。
//
// 调用方（main）必须在做任何事之前调用它：助手实例的唯一职责就是
// 等父进程退出、换掉二进制、可选地重启，然后立刻返回。
func SwapHelperRequested() bool {
	for _, a := range os.Args[1:] {
		if a == swapHelperFlag {
			return true
		}
	}
	return false
}

// RunSwapHelper 执行助手逻辑并返回进程退出码。
//
// 之所以用「等父进程退出」而不是「等固定秒数」：父进程退出时刻不可预测
// （可能在处理一个长流式请求）。这里靠**能不能改名**来判断它是否已经走了 ——
// 文件一旦不再被占用，改名就会成功。这比读 PID / 轮询任务列表更可靠，
// 因为它直接测的就是我们真正关心的那个前置条件。
func RunSwapHelper() int {
	src, dst := "", ""
	relaunch := true
	var relaunchArgs []string

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case swapSrcArg:
			if i+1 < len(args) {
				src = args[i+1]
				i++
			}
		case swapDstArg:
			if i+1 < len(args) {
				dst = args[i+1]
				i++
			}
		case swapNoRelaunchArg:
			relaunch = false
		case swapRelaunchArg:
			if i+1 < len(args) {
				relaunchArgs = append(relaunchArgs, args[i+1])
				i++
			}
		}
	}
	if src == "" || dst == "" {
		return 2
	}

	deadline := time.Now().Add(swapHelperTimeout)
	for {
		if err := os.Rename(src, dst); err == nil {
			break
		}
		if time.Now().After(deadline) {
			// 超时说明父进程一直没退出，或没有权限。保持原样退出，
			// 绝不删源文件 —— 留着它下次还能用，删了就彻底没得救。
			return 1
		}
		time.Sleep(swapPollInterval)
	}

	_ = os.Chmod(dst, 0o755)

	if !relaunch {
		return 0
	}

	// 稍等片刻再拉起：旧进程刚退出时端口/TCP 连接可能还在
	// TIME_WAIT，立刻重启会 bind 失败。当前进程用的是可复用的监听，
	// 一般不受影响，这个延迟是纯保险。
	time.Sleep(swapSettleDelay)
	_ = spawnConsole(dst, relaunchArgs)
	return 0
}

func dirOf(p string) string {
	d := filepath.Dir(p)
	if d == "" {
		return "."
	}
	return d
}
