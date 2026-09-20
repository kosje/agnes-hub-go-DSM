//go:build windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewConsole      = 0x00000010
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// replaceBinary 在 Windows 上完成「替换正在运行的 exe」。
//
// Windows 与 Linux 的关键差别：正在执行的映像文件被内核锁定，
// 既不能删除也不能改名，所以**在进程活着的时候替换一定失败**。
// 这意味着不能「下载完就地换掉」，只能：先把新文件放好，
// 等进程退出后再由另一个进程来换。
//
// 这就是这里要派生一个助手进程的原因。旧实现写了个 .bat 后用 `start /B` 拉起，
// 有两个致命问题，导致更新在 Windows 上**静默失效**：
//  1. bat 立刻执行，此时本进程还活着，del/move 必然失败；
//  2. bat 是父进程的子进程，父进程一退就被一起带走，也就再没人执行替换。
//
// 现在改为「自调用助手」：用同一个 exe 加内部标记拉起一个脱离控制台的子进程，
// 它等本进程消失后再换文件。用自身而不是 .bat，还顺带绕开了
// 中文路径下 bat 必须存成 GBK 否则被 cmd 按字节切错位的经典坑 ——
// 参数由 argv 传递，不存在代码页解析问题。
func replaceBinary(current, newBin string, noRelaunch bool) error {
	// 万一目标没被占用（例如进程不是从该路径启动的），直接换掉最省事。
	if err := os.Rename(newBin, current); err == nil {
		return nil
	}

	self, err := os.Executable()
	if err != nil || self == "" {
		self = current
	}

	args := []string{swapHelperFlag, swapSrcArg, newBin, swapDstArg, current}
	if noRelaunch {
		args = append(args, swapNoRelaunchArg)
	}
	// 把本次启动的参数原样带上，否则重启后会丢掉 -host/-port/-data，
	// 新版本会以默认端口起来，看起来就像「更新后服务不见了」。
	for _, a := range os.Args[1:] {
		args = append(args, swapRelaunchArg, a)
	}

	cmd := exec.Command(self, args...)
	cmd.Dir = dirOf(current)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// 三者都必要：
		//   DETACHED_PROCESS       —— 脱离父控制台，父进程退出不会带走它；
		//   CREATE_NEW_PROCESS_GROUP —— 不接收父进程的 Ctrl+C；
		//   CREATE_NO_WINDOW       —— 助手是控制台程序，不加会闪一个黑窗。
		CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动替换助手失败：%w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

// spawnConsole 用**独立的新控制台窗口**拉起新版本。
//
// 刻意不复用 DETACHED_PROCESS：那样服务会在没有任何窗口的情况下跑起来，
// 用户既看不到日志也关不掉，比更新失败还糟。给一个新控制台，
// 体验就和原来双击启动脚本一致。
func spawnConsole(path string, args []string) error {
	cmd := exec.Command(path, args...)
	cmd.Dir = dirOf(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
	return cmd.Start()
}
