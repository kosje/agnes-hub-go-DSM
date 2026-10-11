package web

import (
	"net/http"
	"net/url"
	"strings"
)

func validImageBytes(raw []byte) bool {
	_, ok := imageKind.extByMIME[normalizeMIME(http.DetectContentType(raw))]
	return ok
}

func validImageResult(raw []byte) bool {
	payload := decodeMap(raw)
	if payload["error"] != nil {
		return false
	}
	for _, item := range mapList(payload["data"]) {
		if raw, ok := item["url"].(string); ok && strings.TrimSpace(raw) != "" {
			u, err := url.Parse(strings.TrimSpace(raw))
			if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" {
				return true
			}
			if strings.HasPrefix(raw, "data:image/") {
				if data, err := decodeBase64Loose(raw); err == nil && validImageBytes(data) {
					return true
				}
			}
		}
		if b64, ok := item["b64_json"].(string); ok && b64 != "" {
			if data, err := decodeBase64Loose(b64); err == nil && validImageBytes(data) {
				return true
			}
		}
	}
	return false
}

func validVideoResult(raw []byte) bool {
	payload := decodeMap(raw)
	return payload["error"] == nil && extractVideoID(payload) != ""
}

func invalidMediaResponse() *apiError {
	return &apiError{Status: 502, Type: "upstream_error", Code: "invalid_media_response", Message: "上游没有返回有效的媒体产出或视频任务号，请重试"}
}
