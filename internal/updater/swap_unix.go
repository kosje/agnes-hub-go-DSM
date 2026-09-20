//go:build !windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
)

// replaceBinary 在类 Unix 上就地替换正在运行的二进制。
//
// 为什么这里可以「边跑边换」：Linux 允许对正在执行的文件做 rename。
// 内核持有旧 inode，当前进程照常跑完；新文件从下一个进程开始生效。
// 所以这一步总是立即成功，剩下的只是让调用方退出进程。
func replaceBinary(current, newBin string, _ bool) error {
	if err := os.Rename(newBin, current); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", newBin, current, err)
	}
	_ = os.Chmod(current, 0o755)
	return nil
}

// spawnConsole 在类 Unix 上以「继承当前终端」的方式拉起新进程。
// 只在 Windows 的替换流程里被调用，这里给出等价实现以便跨平台编译。
func spawnConsole(path string, args []string) error {
	cmd := exec.Command(path, args...)
	cmd.Dir = dirOf(path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}
