// Package pool 负责「模型名 → 池分类（限流桶）」以及「模态 ↔ 池」的换算。
//
// 为什么要有「模态」这一层：agnes-auto 的整个玩法是「客户端不给模型名，
// 由网关判定意图 → 决定模态 → 再从账号声明的模型清单里挑具体模型」。
// 池分类则决定用哪个限流桶（文本池 / 图片各分辨率池 / 视频池）。
package pool

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"agneshub/internal/config"
)

// 已知模型清单。官方文档与线上实际不一致，这里以**实测线上**为准：
// 中国站没有 agnes-1.5-flash（返回 503 model_not_found），反而多出
// agnes-3.0-flash / agnes-2.5-pro* / agnes-image-2.5-flash / agnes-video-2.5*。
var (
	TextModels = map[string]bool{
		"agnes-2.5-flash": true, "agnes-2.0-flash": true, "agnes-1.5-flash": true,
		"agnes-3.0-flash": true, "agnes-2.5-pro": true,
		"agnes-2.5-pro-alpha": true, "agnes-2.5-pro-beta": true,
	}
	ImageModels = map[string]bool{
		"agnes-image-2.0-flash": true, "agnes-image-2.1-flash": true, "agnes-image-2.5-flash": true,
	}
	VideoModels = map[string]bool{
		"agnes-video-v2.0": true, "agnes-video-2.5": true, "agnes-video-2.5-flash": true,
	}
)

// BuiltinAliases 兼容旧客户端的常见别名（控制台 model_aliases 可覆写/追加）。
var BuiltinAliases = map[string]string{
	"gpt-4o":            "agnes-2.5-flash",
	"gpt-4o-mini":       "agnes-2.0-flash",
	"gpt-4-turbo":       "agnes-2.0-flash",
	"gpt-3.5-turbo":     "agnes-1.5-flash",
	"claude-3-5-sonnet": "agnes-2.5-flash",
	"claude-sonnet-4":   "agnes-2.5-flash",
	"dall-e-3":          "agnes-image-2.1-flash",
	"gpt-image-1":       "agnes-image-2.1-flash",
}

var (
	tierRe = regexp.MustCompile(`(?i)^\s*([1-4])\s*k\s*$`)
	dimRe  = regexp.MustCompile(`(?i)^\s*(\d{2,5})\s*[x×*]\s*(\d{2,5})\s*$`)
)

// tierByLongEdge 长边 → 档位。官方示例：1K/16:9=1312x736，2K/16:9=2624x1472。
var tierByLongEdge = []struct {
	max  int
	tier string
}{{1500, "1k"}, {2800, "2k"}, {3500, "3k"}}

// ModalityByPool 池分类 → 模态。
var ModalityByPool = map[string]string{
	"text":     "text",
	"image_1k": "image", "image_2k": "image", "image_3k": "image", "image_4k": "image",
	"video": "video",
}

// FallbackModel 每个模态的兜底模型（仅在账号清单与偏好都为空时使用）。
var FallbackModel = map[string]string{
	"text":  "agnes-3.0-flash",
	"image": "agnes-image-2.5-flash",
	"video": "agnes-video-2.5-flash",
}

// ResolveModel 把下游传来的模型名解析成上游 agnes 原生模型名。
func ResolveModel(model string, aliases map[string]string) string {
	name := strings.TrimSpace(model)
	if name == "" {
		return ""
	}
	if aliases != nil {
		if v, ok := aliases[name]; ok && v != "" {
			return v
		}
	}
	if v, ok := BuiltinAliases[name]; ok {
		return v
	}
	return name
}

// DetectImageTier 从请求体推断图片档位（1k/2k/3k/4k），拿不准时归入更严的档。
func DetectImageTier(body map[string]any, defaultTier string) string {
	if body != nil {
		if size, ok := body["size"].(string); ok {
			if m := tierRe.FindStringSubmatch(size); m != nil {
				return strings.ToLower(m[1]) + "k"
			}
			if d := dimRe.FindStringSubmatch(size); d != nil {
				w, _ := strconv.Atoi(d[1])
				h, _ := strconv.Atoi(d[2])
				long := w
				if h > long {
					long = h
				}
				for _, t := range tierByLongEdge {
					if long <= t.max {
						return t.tier
					}
				}
				return "4k"
			}
		}
		w := toInt(body["width"])
		h := toInt(body["height"])
		if w > 0 && h > 0 {
			long := w
			if h > long {
				long = h
			}
			for _, t := range tierByLongEdge {
				if long <= t.max {
					return t.tier
				}
			}
			return "4k"
		}
	}
	tier := strings.ToLower(strings.TrimSpace(defaultTier))
	switch tier {
	case "1k", "2k", "3k", "4k":
		return tier
	}
	return "1k"
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		return i
	}
	return 0
}

// ModalityOfModel 按模型名判定模态；无法判定返回空串。
func ModalityOfModel(model string, aliases map[string]string) string {
	resolved := ResolveModel(model, aliases)
	switch {
	case resolved == "":
		return ""
	case ImageModels[resolved]:
		return "image"
	case VideoModels[resolved]:
		return "video"
	case TextModels[resolved]:
		return "text"
	}
	low := strings.ToLower(resolved)
	if strings.Contains(low, "image") {
		return "image"
	}
	if strings.Contains(low, "video") {
		return "video"
	}
	return ""
}

// Classify 返回池分类：text / image_1k..image_4k / video。
func Classify(model string, body map[string]any, aliases map[string]string, defaultTier string) string {
	resolved := ResolveModel(model, aliases)
	if ImageModels[resolved] {
		return "image_" + DetectImageTier(body, defaultTier)
	}
	if VideoModels[resolved] {
		return "video"
	}
	if TextModels[resolved] {
		return "text"
	}
	low := strings.ToLower(resolved)
	if strings.Contains(low, "image") {
		return "image_" + DetectImageTier(body, defaultTier)
	}
	if strings.Contains(low, "video") {
		return "video"
	}
	return "text"
}

// ModalityOfPool 池分类 → 模态。
func ModalityOfPool(poolClass string) string {
	if m, ok := ModalityByPool[poolClass]; ok {
		return m
	}
	return "text"
}

// PoolForModality 模态 → 池分类（图片按分辨率落桶）。
func PoolForModality(modality string, body map[string]any, defaultTier string) string {
	switch modality {
	case "image":
		return "image_" + DetectImageTier(body, defaultTier)
	case "video":
		return "video"
	default:
		return "text"
	}
}

// ManifestOf 读取账号声明的清单；缺失（nil）时用默认清单补齐。
// 注意：**非 nil 的空切片会被保留**，表示「明示不支持该模态」。
func ManifestOf(a *config.Account, s config.Settings) config.ModelManifest {
	dm := config.DefaultManifestForType(a.AccessType, s)
	if a.ModelManifest.Text == nil && a.ModelManifest.Image == nil && a.ModelManifest.Video == nil {
		return dm.Clone()
	}
	out := a.ModelManifest.Clone()
	if out.Text == nil {
		out.Text = append([]string(nil), dm.Text...)
	}
	if out.Image == nil {
		out.Image = append([]string(nil), dm.Image...)
	}
	if out.Video == nil {
		out.Video = append([]string(nil), dm.Video...)
	}
	return out
}

// KnownModelNames 返回用于 /v1/models 的模型目录。
func KnownModelNames(aliases map[string]string) []string {
	set := map[string]bool{}
	for m := range TextModels {
		set[m] = true
	}
	for m := range ImageModels {
		set[m] = true
	}
	for m := range VideoModels {
		set[m] = true
	}
	for m := range aliases {
		set[m] = true
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// KnownModelNamesByModality 按模态返回已知的模型名（含别名）。
func KnownModelNamesByModality(aliases map[string]string) map[string][]string {
	out := map[string][]string{"text": {}, "image": {}, "video": {}}
	seen := map[string]bool{}
	add := func(name, modality string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := modality + "|" + name
		if seen[key] {
			return
		}
		seen[key] = true
		out[modality] = append(out[modality], name)
	}

	// 标准模型（Agnes 原生模型）
	for m := range TextModels {
		add(m, "text")
	}
	for m := range ImageModels {
		add(m, "image")
	}
	for m := range VideoModels {
		add(m, "video")
	}

	// 用户自定义别名（控制台 model_aliases 里配置的）
	for alias, target := range aliases {
		if mod := ModalityOfModel(target, aliases); mod != "" {
			add(alias, mod)
		}
	}

	// 注意：不返回 BuiltinAliases（gpt-4o/claude/dall-e-3/gpt-image-1 等），
	// 这些跨厂商兼容别名仅用于 /v1/models 和外部客户端兼容，
	// 聊天 UI 只展示用户实际接入的 Agnes 模型与用户自定义别名。

	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
