package intent

import (
	"strings"
	"testing"
)

// TestIsVideo25 钉住两个视频家族的识别。
//
// 上游的命名有个坑：v2.0 是 `agnes-video-v2.0`（含 "2.0"），2.5 是
// `agnes-video-2.5`（含 "2.5"）。只按 "v2" 前缀判断会把 v2.0 误判成 2.5。
func TestIsVideo25(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"agnes-video-2.5", true},
		{"agnes-video-2.5-flash", true},
		{"agnes-video-v2.0", false},
		{"agnes-video-2.0", false},
		{"agnes-video-2_5", true},
		// 认不出来的自定义别名：按 2.5 处理（带 mode 比不带更容易成功）
		{"my-custom-video", true},
		{"", true},
	}
	for _, c := range cases {
		if got := IsVideo25(c.model); got != c.want {
			t.Errorf("IsVideo25(%q) = %v，期望 %v", c.model, got, c.want)
		}
	}
}

// TestBuildVideoBody25AlwaysSendsMode 钉住这次的真实故障。
//
// 用户选 agnes-auto 生视频，上游回 `mode is required`：2.5 系列把 mode 列为必填，
// 而网关的字段白名单里根本没有它。以前这条链路必然失败。
func TestBuildVideoBody25AlwaysSendsMode(t *testing.T) {
	for _, model := range []string{"agnes-video-2.5", "agnes-video-2.5-flash"} {
		got := BuildVideoBody(Result{Prompt: Prompt{Text: "海边的日落"}}, model, Config{}, nil)
		if got["mode"] != VideoModeText {
			t.Errorf("%s：纯文本请求应补上 mode=%s，实际 %v", model, VideoModeText, got["mode"])
		}
		if got["prompt"] != "海边的日落" {
			t.Errorf("%s：提示词应保留，实际 %v", model, got["prompt"])
		}
	}
}

// TestBuildVideoBody25RejectsLegacyParams 2.5 明确禁止老一代的不可配置字段。
//
// 白名单以前全是 v2.0 味的（width/height/num_frames/frame_rate），照抄过去
// 上游会直接 400 —— 就算补上 mode 也还是不通。
func TestBuildVideoBody25RejectsLegacyParams(t *testing.T) {
	source := map[string]any{
		"width": 1152, "height": 768, "num_frames": 121, "frame_rate": 24.0,
		"duration": 8,
	}
	got := BuildVideoBody(Result{Prompt: Prompt{Text: "x"}}, "agnes-video-2.5-flash", Config{}, source)
	for _, k := range []string{"width", "height", "num_frames", "frame_rate"} {
		if _, ok := got[k]; ok {
			t.Errorf("2.5 不该收到 %s（上游判定为不可配置字段），实际 %v", k, got[k])
		}
	}
	// duration 要归一成 seconds（字符串），2.5 不认 duration
	if got["seconds"] != "8" {
		t.Errorf("duration 应归一成 seconds=\"8\"，实际 %v（全量 %v）", got["seconds"], got)
	}
	if _, ok := got["duration"]; ok {
		t.Errorf("不该把 duration 原样透传，实际 %v", got["duration"])
	}
}

// TestBuildVideoBody25ModeInference 按调用方提供的素材推断 mode。
//
// 上游对「mode 与媒体字段不匹配」是直接 400，所以推断必须和素材一致。
func TestBuildVideoBody25ModeInference(t *testing.T) {
	cases := []struct {
		name   string
		source map[string]any
		res    Result
		want   string
	}{
		{"无素材 → text", nil, Result{Prompt: Prompt{Text: "x"}}, VideoModeText},
		{"有首帧 → keyframe", map[string]any{"first_frame": "https://a/1.png"},
			Result{Prompt: Prompt{Text: "x"}}, VideoModeKeyframe},
		{"有尾帧 → keyframe", map[string]any{"last_frame": "https://a/2.png"},
			Result{Prompt: Prompt{Text: "x"}}, VideoModeKeyframe},
		{"有参考图 → reference", map[string]any{"images": []any{"https://a/1.png"}},
			Result{Prompt: Prompt{Text: "x"}}, VideoModeReference},
		{"提示词里带图 → reference", nil,
			Result{Prompt: Prompt{Text: "x", Images: []string{"https://a/1.png"}}}, VideoModeReference},
		{"显式 mode 且与素材一致 → 尊重", map[string]any{"mode": "reference", "images": []any{"u"}},
			Result{Prompt: Prompt{Text: "x"}}, VideoModeReference},
		// 素材优先：说 text 却塞了首帧图，按 text 发上游会拒
		{"显式 mode 与素材冲突 → 素材优先", map[string]any{"mode": "text", "first_frame": "u"},
			Result{Prompt: Prompt{Text: "x"}}, VideoModeKeyframe},
	}
	for _, c := range cases {
		got := BuildVideoBody(c.res, "agnes-video-2.5", Config{}, c.source)
		if got["mode"] != c.want {
			t.Errorf("%s：mode = %v，期望 %v（全量 %v）", c.name, got["mode"], c.want, got)
		}
	}
}

// TestBuildVideoBody25SanitizesConflictingMedia 与 mode 冲突的媒体字段必须清掉，
// 否则上游以「mode 与媒体字段不匹配」400。
func TestBuildVideoBody25SanitizesConflictingMedia(t *testing.T) {
	// keyframe 模式下不允许 images
	got := BuildVideoBody(Result{Prompt: Prompt{Text: "x"}}, "agnes-video-2.5", Config{},
		map[string]any{"mode": "keyframe", "first_frame": "u1", "images": []any{"u2"}})
	if got["mode"] != VideoModeKeyframe {
		t.Fatalf("mode 应为 keyframe，实际 %v", got["mode"])
	}
	if _, ok := got["images"]; ok {
		t.Errorf("keyframe 模式下不该带 images，实际 %v", got["images"])
	}
	if got["first_frame"] != "u1" {
		t.Errorf("首帧应保留，实际 %v", got["first_frame"])
	}

	// reference 模式下不允许 first_frame / last_frame
	got2 := BuildVideoBody(Result{Prompt: Prompt{Text: "x"}}, "agnes-video-2.5", Config{},
		map[string]any{"mode": "reference", "first_frame": "u1", "images": []any{"u2"}})
	if _, ok := got2["first_frame"]; ok {
		t.Errorf("reference 模式下不该带 first_frame，实际 %v", got2["first_frame"])
	}
	if _, ok := got2["images"]; !ok {
		t.Errorf("参考图应保留，实际 %v", got2)
	}
}

// TestBuildVideoBodyV20KeepsLegacyParams v2.0 走老口径：width/num_frames 照常透传，
// 不能因为 2.5 的规则把它们误删。
func TestBuildVideoBodyV20KeepsLegacyParams(t *testing.T) {
	source := map[string]any{"width": 1152, "height": 768, "num_frames": 121, "frame_rate": 24.0}
	got := BuildVideoBody(Result{Prompt: Prompt{Text: "x"}}, "agnes-video-v2.0", Config{}, source)
	for _, k := range []string{"width", "height", "num_frames", "frame_rate"} {
		if _, ok := got[k]; !ok {
			t.Errorf("v2.0 应保留 %s，实际 %v", k, got)
		}
	}
	// v2.0 的 mode 是可选的，不该被强塞一个 2.5 的枚举值
	if m, ok := got["mode"]; ok && m == VideoModeText {
		t.Errorf("v2.0 不该被塞 2.5 的 mode 枚举，实际 %v", m)
	}
}

// TestVideoFieldsForReportsPerFamily 字段白名单要按家族返回。
//
// 否则往 2.5 发请求时会把 mode/seconds/size 这些上游真正需要的字段
// 报成「已忽略」，既误导用户，也让真正的丢弃淹没在噪音里。
func TestVideoFieldsForReportsPerFamily(t *testing.T) {
	f25 := strings.Join(VideoFieldsFor("agnes-video-2.5-flash"), ",")
	if !strings.Contains(f25, "seconds") || !strings.Contains(f25, "aspect_ratio") {
		t.Errorf("2.5 的白名单应含 seconds / aspect_ratio，实际 %s", f25)
	}
	if strings.Contains(f25, "num_frames") {
		t.Errorf("2.5 的白名单不该含 num_frames，实际 %s", f25)
	}
	v20 := strings.Join(VideoFieldsFor("agnes-video-v2.0"), ",")
	if !strings.Contains(v20, "num_frames") {
		t.Errorf("v2.0 的白名单应含 num_frames，实际 %s", v20)
	}
	if strings.Contains(v20, "seconds") {
		t.Errorf("v2.0 的白名单不该含 seconds，实际 %s", v20)
	}
}
