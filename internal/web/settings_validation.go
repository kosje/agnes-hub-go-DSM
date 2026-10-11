package web

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"agneshub/internal/config"
)

// Validate before mutating any field so a rejected patch cannot partially save.
func validateSettingsPatch(p map[string]any) *apiError {
	if value, ok := p["scenario"]; ok {
		if _, valid := config.FindScenario(asStr(value)); !valid {
			return badRequest("未知的运行情景")
		}
	}
	for key, allowed := range map[string][]string{
		"client_media_url": {"", "auto", "local", "upstream"},
		"routing_mode":     config.RoutingModes,
	} {
		if value, exists := p[key]; exists {
			valid := false
			for _, choice := range allowed {
				if asStr(value) == choice {
					valid = true
				}
			}
			if !valid {
				return badRequest(key + " 的取值无效")
			}
		}
	}
	if value, exists := p["public_base_url"]; exists {
		raw, ok := value.(string)
		if !ok {
			return badRequest("接口地址必须为字符串")
		}
		raw = strings.TrimSpace(raw)
		if raw != "" {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
				return badRequest("接口地址必须是完整的 http:// 或 https:// 地址，不含账号、查询参数或片段")
			}
			if u.Port() != "" {
				port, err := strconv.Atoi(u.Port())
				if err != nil || port < 1 || port > 65535 {
					return badRequest("接口地址端口必须为 1～65535")
				}
			}
			raw = u.String()
		}
		p["public_base_url"] = strings.TrimRight(raw, "/")
	}
	for _, key := range []string{"request_timeout_ms", "stream_idle_timeout_ms", "continuation_max_rounds", "soft_fail_retry_max", "rate_limit_retry_max", "rate_limit_wait_budget_ms", "video_cache_max_mb", "image_max_capacity", "video_max_capacity", "image_record_retention_days", "retry_max", "log_retention_days", "usage_request_log_bytes"} {
		if value, ok := p[key]; ok {
			n, err := strconv.ParseFloat(fmt.Sprint(value), 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > math.MaxInt32 || math.Trunc(n) != n {
				return badRequest(key + " 必须为非负整数")
			}
		}
	}
	if value, ok := p["usage_log_max_lines"]; ok {
		n, err := strconv.ParseFloat(fmt.Sprint(value), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < -1 || n > math.MaxInt32 || math.Trunc(n) != n {
			return badRequest("日志行数必须为整数，-1 表示不限")
		}
	}
	if value, ok := p["free_text_rpm"]; ok {
		n, err := strconv.ParseFloat(fmt.Sprint(value), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
			return badRequest("free_text_rpm 必须为正数")
		}
	}
	return nil
}

func validateBudgetPatch(p map[string]any) *apiError {
	if value, ok := p["daily_budget"]; ok {
		n, err := strconv.ParseFloat(fmt.Sprint(value), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return badRequest("每日用量预算必须为非负数")
		}
	}
	if value, ok := p["budget_unit"]; ok && asStr(value) != "" && asStr(value) != "neurons" {
		return badRequest("当前日预算支持 neurons 用量单位")
	}
	return nil
}
