package intent

import "testing"

// TestIsAutoModelNamed 钉住「让网关决定」的名称识别。
//
// 回归背景：IsAutoModel 只认内置的 auto / auto-all / auto* 前缀，而本项目的
// 默认 AutoModelName 是 "agnes-auto" —— 不在内置集合里。只调用 IsAutoModel
// 会把 agnes-auto 判成「显式模型」，整条自动判定链路（内容判定 → 生图/生视频）
// 被跳过，请求带着 model=agnes-auto 原样透传给上游。
// 实测表现：对话页模型选 agnes-auto 说「画一幅山水画」，界面回「（无内容返回）」。
func TestIsAutoModelNamed(t *testing.T) {
	cases := []struct {
		name, autoName string
		want           bool
	}{
		// 默认配置：这正是之前漏掉的那一个
		{"agnes-auto", "agnes-auto", true},
		// 大小写与空白要归一
		{"AGNES-AUTO", "agnes-auto", true},
		{"  agnes-auto  ", "agnes-auto", true},
		// 内置名与自定义名仍然认
		{"auto", "agnes-auto", true},
		{"auto-all", "agnes-auto", true},
		{"auto-fast", "agnes-auto", true},
		{"my-auto", "my-auto", true},
		// 显式模型名绝不能被误判成 auto，否则显式路由会被内容判定劫持
		{"agnes-image-2.5-flash", "agnes-auto", false},
		{"agnes-video-2.5-flash", "agnes-auto", false},
		{"agnes-3.0-flash", "agnes-auto", false},
		{"gpt-4o", "agnes-auto", false},
		// 没有配置 auto 名时，不额外认任何东西
		{"agnes-auto", "", false},
		{"", "agnes-auto", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := IsAutoModelNamed(c.name, c.autoName); got != c.want {
			t.Errorf("IsAutoModelNamed(%q, %q) = %v，期望 %v",
				c.name, c.autoName, got, c.want)
		}
	}
}

// TestDecideStillScansContentForConfiguredAutoName 确认 Decide 内部不会因为
// 「agnes-auto 不在内置 auto 集合里」而把模态锁死成 text。
//
// Decide 的 model 分支只会在 modalityOfModel 返回非空时锁定；agnes-auto 不是
// 已知模型名，返回空，所以会继续走内容判定 —— 这条断言把这个前提钉住，
// 避免以后有人往 pool 的模型表里加了 agnes-auto 而悄悄改掉路由行为。
func TestDecideStillScansContentForConfiguredAutoName(t *testing.T) {
	body := map[string]any{"model": "agnes-auto", "messages": []any{
		map[string]any{"role": "user", "content": "画一幅山水画。"}}}

	res := Decide("/v1/chat/completions", body, "agnes-auto",
		cfg(), DefaultRules(), nil, func(string, map[string]string) string { return "" }, "")

	if res.Modality != Image {
		t.Fatalf("agnes-auto + 画图内容应判为 image，实际 %s（source=%s score=%.2f reason=%s）",
			res.Modality, res.Source, res.Score, res.Reason)
	}
	if res.Source != "content" {
		t.Errorf("判定依据应为 content，实际 %s", res.Source)
	}
}
