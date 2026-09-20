//go:build windows

package updater

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 这个用例复现自更新在 Windows 上唯一真正棘手的地方：
// 旧进程还活着时，目标 exe 被内核锁着，改名必然失败；
// 助手必须一直等到锁释放，而不是试一次就放弃。
//
// 模拟锁的方式要选对：Go 的 os.OpenFile 默认带
// FILE_SHARE_READ|WRITE|DELETE，那种句柄**不会**阻止改名，
// 用它来模拟会得到一个假通过。真正会挡住改名的是共享模式为 0 的句柄
// （运行中的 exe 映像同理），所以这里直接用 syscall.CreateFile。
func TestRunSwapHelper_WaitsForLockRelease(t *testing.T) {
	origTimeout, origPoll := swapHelperTimeout, swapPollInterval
	swapHelperTimeout, swapPollInterval = 10*time.Second, 25*time.Millisecond
	defer func() { swapHelperTimeout, swapPollInterval = origTimeout, origPoll }()

	dir := t.TempDir()
	src := filepath.Join(dir, "new.bin")
	dst := filepath.Join(dir, "old.bin")
	if err := os.WriteFile(src, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 独占打开 dst：共享模式 0，等价于「旧进程正占着这个文件」。
	p, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("建立独占句柄失败：%v", err)
	}

	// 先确认这个锁真的挡住了改名 —— 否则整个用例是在自我安慰。
	if err := os.Rename(src, dst); err == nil {
		syscall.CloseHandle(h)
		t.Fatal("独占句柄没有挡住重命名，本用例失效，需要换一种锁模拟方式")
	}

	done := make(chan int, 1)
	orig := os.Args
	os.Args = []string{"agnes-hub-go", swapHelperFlag,
		swapSrcArg, src, swapDstArg, dst, swapNoRelaunchArg}
	go func() {
		defer func() { os.Args = orig }()
		done <- RunSwapHelper()
	}()

	// 锁还在时助手应当仍在等，不该提前结束。
	select {
	case code := <-done:
		syscall.CloseHandle(h)
		t.Fatalf("锁未释放助手就结束了（退出码 %d），应当继续等待", code)
	case <-time.After(600 * time.Millisecond):
	}

	syscall.CloseHandle(h)

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("释放锁后助手应成功，实际退出码 %d", code)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("释放锁后助手仍未完成替换")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Errorf("替换后内容应为 NEW，实际 %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("源文件应已被移走")
	}
}
