package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agneshub/internal/config"
	"agneshub/internal/hub"
	"agneshub/internal/relay"
)

func videoChatBody() map[string]any {
	return map[string]any{
		"model":    "agnes-video-2.5-flash",
		"messages": []any{map[string]any{"role": "user", "content": "生成一段仙逆女主角李慕婉的视频。"}},
		"stream":   true,
	}
}

// AI 客户端（流式对话）要视频：同一条回复里先说「生成中」，完成后接上视频链接。
// 旧行为只回任务号和轮询地址，而对话客户端不会去轮询，视频永远出不来。
func TestChatStreamVideoWaitsForResult(t *testing.T) {
	h := newHarness(t, 0, 1)
	serveVideoUpstream(t, h, "video/mp4")
	_ = h.store.UpdateSettings(func(s *config.Settings) { s.VideoPollIntervalMS = 1000 })

	raw := h.postSSE("/v1/chat/completions", videoChatBody())
	for _, want := range []string{"视频生成中", "生成完成", "在浏览器中打开", `"finish_reason":"stop"`, "[DONE]"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("流式回复里应包含 %q，实际：\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "请按上面的轮询地址") {
		t.Fatal("开启等待后不应再让用户自己去轮询")
	}
	jobs := h.store.JobsSnapshot()
	if len(jobs) != 1 || jobs[0].Status != "completed" || !strings.Contains(jobs[0].URL, videoKind.route) {
		t.Fatalf("任务应标记完成并落盘本地副本，实际 %+v", jobs)
	}
}

// fakeVideoUpstream 是一个只会「提交」和「查询」的上游：查询结果由 pollReply 决定。
func fakeVideoUpstream(t *testing.T, pollReply map[string]any) (*config.Store, *config.DownstreamKey, string) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/v1/videos") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "video_x1", "video_id": "video_x1", "status": "queued"})
			return
		}
		_ = json.NewEncoder(w).Encode(pollReply)
	}))
	t.Cleanup(up.Close)
	store := newTempStore(t)
	store.AddAccount("v", "k", "free", up.URL+"/v1",
		&config.ModelManifest{Video: []string{"agnes-video-2.5-flash"}})
	_ = store.UpdateSettings(func(s *config.Settings) {
		s.VideoPollIntervalMS = 1000
		s.AutoIntent.VideoStreamWaitSec = 3
	})
	key := store.AddKey("k", []string{"*"}, 0, 0, "")
	ts := httptest.NewServer(New(store, hub.New(store), relay.BuildClient()))
	t.Cleanup(ts.Close)
	return store, key, ts.URL
}

func postSSERaw(t *testing.T, url, key string, body map[string]any) string {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func TestChatStreamVideoReportsFailure(t *testing.T) {
	store, key, base := fakeVideoUpstream(t, map[string]any{
		"status": "failed", "error": map[string]any{"message": "内容审核未通过"}})
	raw := postSSERaw(t, base+"/v1/chat/completions", key.Key, videoChatBody())
	if !strings.Contains(raw, "生成失败") || !strings.Contains(raw, "内容审核未通过") || !strings.Contains(raw, "[DONE]") {
		t.Fatalf("上游失败应在同一条回复里说明原因，实际：\n%s", raw)
	}
	if jobs := store.JobsSnapshot(); len(jobs) != 1 || jobs[0].Status != "failed" {
		t.Fatalf("任务应标记失败，实际 %+v", jobs)
	}
}

func TestChatStreamVideoTimesOutGracefully(t *testing.T) {
	_, key, base := fakeVideoUpstream(t, map[string]any{"status": "processing"})
	raw := postSSERaw(t, base+"/v1/chat/completions", key.Key, videoChatBody())
	if !strings.Contains(raw, "仍未完成") || !strings.Contains(raw, "后台继续") || !strings.Contains(raw, "[DONE]") {
		t.Fatalf("等到上限应说明任务仍在后台继续并正常结束回复，实际：\n%s", raw)
	}
}

// 设成 0 就是旧行为：只回任务号，不占连接。
func TestChatStreamVideoWaitCanBeDisabled(t *testing.T) {
	store, key, base := fakeVideoUpstream(t, map[string]any{"status": "processing"})
	_ = store.UpdateSettings(func(s *config.Settings) { s.AutoIntent.VideoStreamWaitSec = 0 })
	raw := postSSERaw(t, base+"/v1/chat/completions", key.Key, videoChatBody())
	if !strings.Contains(raw, "请按上面的轮询地址") || strings.Contains(raw, "视频生成中") {
		t.Fatalf("关闭等待后应退回只回任务号，实际：\n%s", raw)
	}
}
