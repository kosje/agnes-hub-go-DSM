package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agneshub/internal/config"
)

// fakeMP4 是一段假的视频字节。
//
// 不需要真的是合法 MP4：videoKind 的扩展名优先按 URL 后缀决定，
// 这正是要验证的行为（CDN 对视频常回 application/octet-stream，
// 靠 Content-Type 定扩展名会存成 .bin，浏览器就不认了）。
var fakeMP4 = []byte("\x00\x00\x00\x18ftypmp42fake-video-payload-for-tests")

// serveVideoUpstream 起一个假 CDN 提供视频产出，并把它塞给模拟上游。
func serveVideoUpstream(t *testing.T, h *harness, contentType string) {
	t.Helper()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(fakeMP4)
	}))
	t.Cleanup(cdn.Close)
	h.mock.setVideoURL(cdn.URL + "/output.mp4")
}

// TestChatVideoIsDownloadedLocally 钉住诉求「视频按图片的方式落盘」。
//
// 视频完成时不能把上游 CDN 地址直接塞进 <video src>：浏览器不一定够得到，
// 而且那样也没法长期回看。
func TestChatVideoIsDownloadedLocally(t *testing.T) {
	h := newHarness(t, 0, 1)
	serveVideoUpstream(t, h, "application/octet-stream")
	upstream := h.mock.getVideoURL()

	_, submit := h.post("/api/chat/v1/videos", map[string]any{
		"model": "agnes-auto", "prompt": "夕阳下的海滩",
	})
	jobID, _ := submit["job_id"].(string)
	if jobID == "" {
		t.Fatalf("提交应返回 job_id：%v", submit)
	}

	resp, body := h.getJSON("/api/chat/v1/videos/" + jobID)
	if resp.StatusCode != 200 {
		t.Fatalf("轮询应 200，实际 %d：%v", resp.StatusCode, body)
	}
	if got, _ := body["status"].(string); got != "completed" {
		t.Fatalf("应已完成，实际 %q（%v）", got, body)
	}
	// 外部客户端拿到的是上游地址 —— 实测只有公网 https 的产出地址在客户端里
	// 显示得出来，指向 NAS 的 http 地址会退化成一段文字。
	if u, _ := body["video_url"].(string); u != upstream {
		t.Errorf("客户端应拿到上游地址 %q，实际 %q", upstream, u)
	}

	// 但本地副本必须真的落了盘（控制台与聊天记录长期回看靠它）
	job, ok := h.store.JobByID(jobID)
	if !ok || !strings.Contains(job.URL, videoKind.route) {
		t.Fatalf("任务记录里应有本地地址，实际 %+v", job)
	}
	if !strings.HasSuffix(job.URL, ".mp4") {
		t.Errorf("应保留 .mp4 扩展名，实际 %q", job.URL)
	}
	name := job.URL[strings.LastIndex(job.URL, "/")+1:]
	raw, err := os.ReadFile(filepath.Join(h.srv.mediaDir(videoKind), name))
	if err != nil {
		t.Fatalf("视频应已落盘，读取失败 %v", err)
	}
	if !bytes.Equal(raw, fakeMP4) {
		t.Error("落盘内容与上游不一致")
	}
}

// TestChatVideoRouteServesFile 本地视频地址要能真的播起来。
func TestChatVideoRouteServesFile(t *testing.T) {
	h := newHarness(t, 0, 1)
	serveVideoUpstream(t, h, "video/mp4")

	_, submit := h.post("/api/chat/v1/videos", map[string]any{
		"model": "agnes-auto", "prompt": "夕阳下的海滩",
	})
	jobID, _ := submit["job_id"].(string)
	h.getJSON("/api/chat/v1/videos/" + jobID)
	job, _ := h.store.JobByID(jobID)
	local := job.URL
	if !strings.Contains(local, videoKind.route) {
		t.Fatalf("应先落盘，实际 %q", local)
	}

	resp, err := http.Get(local) // job.URL 存的是本地地址（控制台/聊天页用）
	if err != nil {
		t.Fatalf("取本地视频失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("本地视频应 200，实际 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "video/mp4") {
		t.Errorf("Content-Type 应为 video/mp4，实际 %q", ct)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, fakeMP4) {
		t.Error("回放的视频内容与上游不一致")
	}
}

// TestVideoRouteSupportsRange 视频必须支持 Range —— 否则进度条拖不动。
// 靠的是 http.ServeContent（自带 Range 处理），这里钉住别被换成 io.Copy。
func TestVideoRouteSupportsRange(t *testing.T) {
	h := newHarness(t, 0, 1)
	serveVideoUpstream(t, h, "video/mp4")

	_, submit := h.post("/api/chat/v1/videos", map[string]any{
		"model": "agnes-auto", "prompt": "x",
	})
	jobID, _ := submit["job_id"].(string)
	h.getJSON("/api/chat/v1/videos/" + jobID)
	job, _ := h.store.JobByID(jobID)

	req, _ := http.NewRequest(http.MethodGet, job.URL, nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Range 请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("带 Range 应回 206，实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 4 {
		t.Errorf("应只回 4 字节，实际 %d 字节", len(got))
	}
}

// TestVideoLocalizeFallsBackToUpstream CDN 取不到时退回上游地址，
// 不能让整个轮询失败。
func TestVideoLocalizeFallsBackToUpstream(t *testing.T) {
	s := &Server{Store: newTempStore(t)}
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer dead.Close()

	got := s.localizeVideoURL(context.Background(), dead.URL+"/a.mp4", "")
	if got != dead.URL+"/a.mp4" {
		t.Errorf("取不到时应原样退回上游地址，实际 %q", got)
	}
	// 非 http(s) 与已是本地地址都不该被处理
	if got := s.localizeVideoURL(context.Background(), videoKind.route+"abc.mp4", ""); got != videoKind.route+"abc.mp4" {
		t.Errorf("已是本地地址应原样返回，实际 %q", got)
	}
}

// TestVideoKindExtPicking 视频的扩展名判定顺序。
//
// URL 后缀必须优先于 Content-Type：CDN 给视频常回 application/octet-stream，
// 只认 Content-Type 会存成 .bin，浏览器就当下载处理了。
func TestVideoKindExtPicking(t *testing.T) {
	cases := []struct {
		name, rawURL, contentType string
		want                      string
	}{
		{"URL 后缀优先于错误 Content-Type", "https://cdn/a/b.mp4", "application/octet-stream", ".mp4"},
		{"URL 带查询串也要认后缀", "https://cdn/a/b.webm?token=x", "text/plain", ".webm"},
		{"URL 没后缀时看 Content-Type", "https://cdn/a/abc", "video/quicktime", ".mov"},
		{"都认不出时不落盘", "https://cdn/a/abc", "application/octet-stream", ""},
	}
	for _, c := range cases {
		if got := videoKind.pickExt(c.rawURL, c.contentType, nil); got != c.want {
			t.Errorf("%s：pickExt(%q, %q) = %q，期望 %q", c.name, c.rawURL, c.contentType, got, c.want)
		}
	}
}

// TestValidMediaNamePerKind 文件名校验必须按类别来：图片名不能当视频用。
func TestValidMediaNamePerKind(t *testing.T) {
	hex32 := "0123456789abcdef0123456789abcdef"
	if !validMediaName(videoKind, hex32+".mp4") {
		t.Error("合法的视频名应被接受")
	}
	if !validMediaName(videoKind, hex32+".webm") {
		t.Error(".webm 也应被接受")
	}
	if validMediaName(videoKind, hex32+".png") {
		t.Error("图片扩展名不该通过视频的校验")
	}
	if validMediaName(imageKind, hex32+".mp4") {
		t.Error("视频扩展名不该通过图片的校验")
	}
	// 路径穿越
	for _, n := range []string{"../settings.json", "/etc/passwd", hex32 + ".mp4/../x"} {
		if validMediaName(videoKind, n) {
			t.Errorf("%q 应被拒绝", n)
		}
	}
}

// TestPruneMediaCacheByBytes 视频缓存必须同时受「条数」和「总字节」两个约束。
//
// 只卡条数的话，200 条 1080P 视频能吃掉好几个 GB。
func TestPruneMediaCacheByBytes(t *testing.T) {
	store := newTempStore(t)
	s := &Server{Store: store}
	dir := s.mediaDir(videoKind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	// 5 个各 1 MB 的文件
	blob := bytes.Repeat([]byte{0xAB}, 1<<20)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("%032x.mp4", i)
		if err := os.WriteFile(filepath.Join(dir, name), blob, 0o644); err != nil {
			t.Fatalf("写文件失败：%v", err)
		}
	}
	// 上限 2 MB、条数很宽松 → 应该只剩 2 个
	_ = store.UpdateSettings(func(st *config.Settings) {
		st.VideoCacheMaxMB = 2
		st.VideoMaxCapacity = 100
	})
	s.pruneMediaCache(videoKind)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("按总字节应只保留 2 个，实际 %d 个", len(entries))
	}
}

// TestVideoJobPersistsLocalURL 任务记录要存下本地地址，控制台才能直接播放。
func TestVideoJobPersistsLocalURL(t *testing.T) {
	h := newHarness(t, 0, 1)
	serveVideoUpstream(t, h, "video/mp4")

	_, submit := h.post("/api/chat/v1/videos", map[string]any{
		"model": "agnes-auto", "prompt": "x",
	})
	jobID, _ := submit["job_id"].(string)
	h.getJSON("/api/chat/v1/videos/" + jobID)

	job, ok := h.store.JobByID(jobID)
	if !ok {
		t.Fatal("任务应已落库")
	}
	if !strings.Contains(job.URL, videoKind.route) {
		t.Errorf("任务记录里应存本地地址，实际 %q", job.URL)
	}
	if job.SourceURL == "" {
		t.Error("也应记下上游原始地址 —— 外部客户端要的是它")
	}
	// 网页再查一次要能直接拿到本地地址（不再重复回取）
	_, again := h.getJSONWeb("/api/chat/v1/videos/" + jobID)
	if u, _ := again["video_url"].(string); u != job.URL {
		t.Errorf("网页端二次查询应返回本地地址 %q，实际 %q", job.URL, u)
	}
	// 客户端再查一次要拿到上游地址
	_, forClient := h.getJSON("/api/chat/v1/videos/" + jobID)
	if u, _ := forClient["video_url"].(string); u != job.SourceURL {
		t.Errorf("客户端二次查询应返回上游地址 %q，实际 %q", job.SourceURL, u)
	}
}

// getJSONWeb 发一个带「来自网关自家网页」标记的 GET。
func (h *harness) getJSONWeb(path string) (*http.Response, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	if err != nil {
		h.t.Fatalf("构造请求失败：%v", err)
	}
	req.Header.Set(surfaceHeader, "web")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// getJSON 发 GET 并把响应解析成 map。
func (h *harness) getJSON(path string) (*http.Response, map[string]any) {
	h.t.Helper()
	resp, err := http.Get(h.ts.URL + path)
	if err != nil {
		h.t.Fatalf("GET %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// TestRefetchVideoJobBackfillsLocalCopy 「补取」按钮：本地落盘功能上线之前
// 完成的任务没有 url 字段，控制台里点不开，重新问一次上游即可补回本地副本。
func TestRefetchVideoJobBackfillsLocalCopy(t *testing.T) {
	h := newHarness(t, 0, 1)
	serveVideoUpstream(t, h, "video/mp4")

	accs := h.store.AccountsSnapshot()
	if len(accs) == 0 {
		t.Fatal("测试夹具应至少有一个账号")
	}
	// 造一条「旧记录」：已完成、有 video_id、但没有 url
	job := &config.VideoJob{
		JobID: "job-legacy-0001", VideoID: "video_legacy_0001",
		Model: "agnes-video-2.5-flash", AccountID: accs[0].ID,
		Status: "completed", CreatedAt: float64(time.Now().Unix()),
	}
	h.store.PutJob(job)

	req, _ := http.NewRequest(http.MethodPost,
		h.ts.URL+"/api/video-jobs/"+job.JobID+"/refetch", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: h.store.SessionToken()})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("补取请求失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("补取应 200，实际 %d：%s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	u, _ := out["video_url"].(string)
	if !strings.Contains(u, videoKind.route) {
		t.Fatalf("补取后应拿到本地地址，实际 %q（%s）", u, raw)
	}

	// 落库了，且文件真的在
	got, _ := h.store.JobByID(job.JobID)
	if got == nil || !strings.Contains(got.URL, videoKind.route) {
		t.Errorf("任务记录应回写本地地址，实际 %+v", got)
	}
	name := u[strings.LastIndex(u, "/")+1:]
	if _, err := os.Stat(filepath.Join(h.srv.mediaDir(videoKind), name)); err != nil {
		t.Errorf("补取的视频应已落盘：%v", err)
	}
}

// TestRefetchVideoJobRequiresAuth 补取会真的回上游取文件，必须要求管理员会话。
func TestRefetchVideoJobRequiresAuth(t *testing.T) {
	h := newHarness(t, 0, 1)
	resp, err := http.Post(h.ts.URL+"/api/video-jobs/job-x/refetch", "application/json", nil)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("没有管理员会话时不该放行")
	}
}
