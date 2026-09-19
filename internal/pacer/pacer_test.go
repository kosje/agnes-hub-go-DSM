package pacer

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestReserveSpacesByInterval 验证 FIFO 严格节拍：相邻两次放行之间必须隔满一个间隔。
//
// 这是整个限流器的核心不变量。实测依据：Agnes 中国站 22 RPM（间隔 2.73s）7/7 通过，
// 而 30 RPM（间隔 2.0s）第 2 个请求即被限 —— 说明上游约束是「相邻间隔」而不是
// 「60 秒窗口内计数」，所以令牌桶（允许突发）与固定窗口（边界双倍穿透）都不合格。
func TestReserveSpacesByInterval(t *testing.T) {
	p := New(600, 60) // 60/600 = 100ms 间隔
	if p.interval != 100*time.Millisecond {
		t.Fatalf("间隔应为 100ms，实际 %s", p.interval)
	}

	start := time.Now()
	if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("首次领号应立刻成功：%v", err)
	}
	if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("第二次领号应等待后成功：%v", err)
	}
	if elapsed := time.Since(start); elapsed < 95*time.Millisecond {
		t.Fatalf("两次领号应至少间隔 100ms，实际 %s（节拍失效，上游会判定为突发）", elapsed)
	}
}

// TestRejectionDoesNotConsumeSlot 是相对 Python 基线的改进点。
//
// Python 版是「先领号、再等超时报错」，被拒的请求白烧了一个 RPM 槽位 ——
// 表现为「什么都没发出去，额度却没了」。Go 版把准入判定放在加锁区间内完成，
// 被拒时不得改动 nextFreeAt。
func TestRejectionDoesNotConsumeSlot(t *testing.T) {
	p := New(1, 60) // 60s 间隔
	if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("首次领号应成功：%v", err)
	}
	before := p.ProjectedWait()
	if before < 50*time.Second {
		t.Fatalf("占位后预计等待应接近 60s，实际 %s", before)
	}

	if _, err := p.Reserve(context.Background(), time.Second, 0); !errors.Is(err, ErrTooLong) {
		t.Fatalf("预计等待 60s 而上限 1s，应返回 ErrTooLong，实际 %v", err)
	}

	after := p.ProjectedWait()
	if after > before+2*time.Second {
		t.Fatalf("被拒的请求不应占用槽位：拒绝前 %s，拒绝后 %s", before, after)
	}
	if p.Waiting() != 0 {
		t.Fatalf("被拒后排队人数应为 0，实际 %d", p.Waiting())
	}
}

// TestQueueFull 排队人数达到上限时立刻拒绝，且不改变已承诺的节拍。
func TestQueueFull(t *testing.T) {
	p := New(1, 60)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := p.Reserve(ctx, 0, 0); err != nil {
		t.Fatalf("占位失败：%v", err)
	}

	// 第二个请求会真的进入等待（60s），从而把 waiting 抬到 1。
	// 它**有权**占用一个槽位，所以基准值必须在它入队之后再取。
	blocked := make(chan error, 1)
	go func() {
		_, err := p.Reserve(ctx, 0, 0)
		blocked <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for p.Waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Waiting() == 0 {
		t.Fatal("第二个请求未进入排队状态，测试前提不成立")
	}
	before := p.ProjectedWait()

	if _, err := p.Reserve(ctx, 0, 1); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("排队上限为 1 且已有 1 人排队，应返回 ErrQueueFull，实际 %v", err)
	}
	if p.Waiting() != 1 {
		t.Fatalf("队列满被拒不应改变排队人数，实际 %d", p.Waiting())
	}
	if after := p.ProjectedWait(); after > before+2*time.Second {
		t.Fatalf("队列满被拒不应消耗槽位：%s → %s", before, after)
	}

	cancel()
	<-blocked
}

// TestReconfigureClampsBacklog 下调 RPM 后，已排队的请求不应继续背旧账。
//
// 若不夹紧，把 20 RPM 调到 2 RPM 后要等满 60s×队列长度 才见效，
// 控制台上的「校准立即生效」会看起来完全没反应。
func TestReconfigureClampsBacklog(t *testing.T) {
	p := New(1, 60)
	if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("领号失败：%v", err)
	}
	if w := p.ProjectedWait(); w < 50*time.Second {
		t.Fatalf("预期积压约 60s，实际 %s", w)
	}

	p.Reconfigure(60, 60) // 间隔降到 1s
	if w := p.ProjectedWait(); w > 1500*time.Millisecond {
		t.Fatalf("Reconfigure 应把积压夹紧到新间隔量级，实际仍有 %s", w)
	}
	if p.RPM() != 60 {
		t.Fatalf("RPM 应已更新为 60，实际 %v", p.RPM())
	}
}

// TestUnlimitedRPM rpm<=0 视为不限制，不得引入任何人为延迟。
func TestUnlimitedRPM(t *testing.T) {
	p := New(0, 60)
	if p.interval != 0 {
		t.Fatalf("rpm<=0 应视为不限制，实际间隔 %s", p.interval)
	}
	start := time.Now()
	for i := 0; i < 50; i++ {
		if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
			t.Fatalf("第 %d 次领号失败：%v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("不限制时 50 次领号应几乎无延迟，实际 %s", elapsed)
	}
}

// TestContextCancelWhileWaiting 取消上下文应立即返回，且不残留排队计数。
func TestContextCancelWhileWaiting(t *testing.T) {
	p := New(1, 60)
	if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("领号失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.Reserve(ctx, 0, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 context 超时，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消后应立即返回，实际耗时 %s", elapsed)
	}
	if p.Waiting() != 0 {
		t.Fatalf("取消后不应残留排队计数，实际 %d", p.Waiting())
	}
}

// TestConcurrentReserveIsSerialized 并发领号必须被串行化到节拍上，
// 不允许因为加锁顺序以外的原因出现「同一时刻放行多人」。
func TestConcurrentReserveIsSerialized(t *testing.T) {
	const (
		n        = 5
		interval = 100 * time.Millisecond
	)
	p := New(600, 60) // 100ms 间隔

	var wg sync.WaitGroup
	ready := make(chan struct{})
	start := time.Now()
	granted := make([]time.Time, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-ready
			if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
				t.Errorf("第 %d 个请求领号失败：%v", idx, err)
				return
			}
			granted[idx] = time.Now()
		}(i)
	}
	close(ready)
	wg.Wait()

	if elapsed := time.Since(start); elapsed < time.Duration(n-1)*interval {
		t.Fatalf("%d 个并发请求应被摊到 %d 个间隔上（≥ %s），实际 %s",
			n, n-1, time.Duration(n-1)*interval, elapsed)
	}

	// 放行时刻排序后，相邻差值必须不小于一个间隔 ——
	// 即「任何瞬间都不会有两个人同时被放行」。
	times := append([]time.Time(nil), granted...)
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for i := 0; i < n; i++ {
		if times[i].IsZero() {
			t.Fatalf("有请求未拿到放行时刻，测试前提不成立")
		}
	}
	for i := 1; i < n; i++ {
		if d := times[i].Sub(times[i-1]); d < interval-10*time.Millisecond {
			t.Fatalf("第 %d 与第 %d 次放行仅相隔 %s，低于节拍 %s（出现并发穿透）", i-1, i, d, interval)
		}
	}
}

// TestReset 清空积压后应立刻可发。
func TestReset(t *testing.T) {
	p := New(1, 60)
	if _, err := p.Reserve(context.Background(), 0, 0); err != nil {
		t.Fatalf("领号失败：%v", err)
	}
	p.Reset()
	if w := p.ProjectedWait(); w != 0 {
		t.Fatalf("Reset 后不应再有积压，实际 %s", w)
	}
}
