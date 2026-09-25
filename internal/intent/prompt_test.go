package intent

import "testing"

// TestCleanUserTextSkipsFrameworkInjection 钉住这次的真实故障。
//
// AI 客户端会在对话尾部追加自己的框架块（`<craft_mode>` / `<system-reminder>` …），
// 它们同样以 role=user 出现。网关如果直接取「最后一条 user」，就会拿框架指令去生图 ——
// 实测：用户说「帮我画一张水墨国风佳人图片」，上游收到的提示词是
// "<craft_mode>You are now in Agent mode…"，用户的原话根本没送出去。
func TestCleanUserTextSkipsFrameworkInjection(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"纯框架块 → 丢弃", "<craft_mode>You are now in Agent mode.</craft_mode>", ""},
		{"带空格的框架块 → 丢弃", "<craft_mode> You are now in Agent mode.</craft_mode>", ""},
		{"system-reminder → 丢弃", "<system-reminder>Current time is 2026-09-26</system-reminder>", ""},
		{"user_info → 丢弃", "<user_info>IDE Theme: dark</user_info>", ""},
		{"连续多段框架块 → 全丢弃", "<memory>a</memory>\n<task-notification>b</task-notification>", ""},
		{"未闭合的已知框架块 → 丢弃", "<craft_mode>You are now in Agent mode.", ""},
		// <user_query> 包的是真实输入，取内层
		{"user_query 包装 → 取内层", "<user_query>帮我画一张水墨国风佳人图片</user_query>", "帮我画一张水墨国风佳人图片"},
		{"框架块 + user_query → 只留内层", "<system-reminder>x</system-reminder>\n<user_query>画只猫</user_query>", "画只猫"},
		// 正常输入必须原样保留
		{"普通输入不变", "帮我画一张水墨国风佳人图片，比例：9:16。", "帮我画一张水墨国风佳人图片，比例：9:16。"},
		{"框架块在前、正文在后 → 只留正文", "<craft_mode>x</craft_mode> 帮我画只猫", "帮我画只猫"},
		// 不能误伤：正文里出现 HTML 标签是正常的（用户可能贴代码）
		{"正文含 HTML 标签不剥", "把 <div>foo</div> 改成居中", "把 <div>foo</div> 改成居中"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		if got := cleanUserText(c.in); got != c.want {
			t.Errorf("%s：cleanUserText(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// TestExtractPromptPicksRealUserTurn 端到端：从真实客户端形态的 messages 里
// 取到用户真正说的那句话。
func TestExtractPromptPicksRealUserTurn(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding agent."},
			map[string]any{"role": "user", "content": "<user_query>帮我画一张水墨国风佳人图片，比例：9:16。</user_query>"},
			map[string]any{"role": "user", "content": "<craft_mode>You are now in Agent mode.</craft_mode>"},
		},
	}
	got := ExtractPrompt(body)
	if got.Text != "帮我画一张水墨国风佳人图片，比例：9:16。" {
		t.Errorf("应取到用户原话，实际 %q", got.Text)
	}
}

// TestExtractPromptFallsBackWhenAllMeta 全是框架块时退回原文，
// 不能把提示词取成空串（那会让上游以「prompt 不能为空」拒绝）。
func TestExtractPromptFallsBackWhenAllMeta(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "<craft_mode>only meta here</craft_mode>"},
		},
	}
	got := ExtractPrompt(body)
	if got.Text == "" {
		t.Error("全为框架块时不该取成空串")
	}
}

// TestExtractPromptKeepsImagesFromChosenTurn 取文本与取图必须来自同一条消息。
func TestExtractPromptKeepsImagesFromChosenTurn(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "<craft_mode>meta</craft_mode>"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "把这张图变成水彩风格"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://a/1.png"}},
			}},
		},
	}
	got := ExtractPrompt(body)
	if got.Text != "把这张图变成水彩风格" {
		t.Errorf("文本取错：%q", got.Text)
	}
	if len(got.Images) != 1 || got.Images[0] != "https://a/1.png" {
		t.Errorf("输入图应取自同一条消息，实际 %v", got.Images)
	}
}

// TestExtractPromptPlainPromptField 没有 messages 时仍支持裸 prompt 字段。
func TestExtractPromptPlainPromptField(t *testing.T) {
	got := ExtractPrompt(map[string]any{"prompt": "一只在窗台晒太阳的橘猫"})
	if got.Text != "一只在窗台晒太阳的橘猫" {
		t.Errorf("裸 prompt 字段应被取到，实际 %q", got.Text)
	}
}
