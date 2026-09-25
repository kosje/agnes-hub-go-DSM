package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agneshub/internal/updater"
)

// 控制台「版本与自更新」卡片要靠 updateStatus 里的 repo 字段拼「查看全部版本」的跳转地址。
// 套件版（自更新被禁用）以前会漏掉这个字段，前端就退化成写死的上游地址 —— 用户点进去
// 看到的是上游仓库，那里根本没有 SPK。这里把两条分支都钉住。
func TestUpdateStatusRepo(t *testing.T) {
	// 分支一：套件版 —— 自更新未启用，repo 必须回落到 main 注入的套件仓库
	s := &Server{Version: "1.0.13", ReleaseRepo: "kosje/agnes-hub-go-DSM"}
	got := s.updateStatus()
	if got["enabled"] != false {
		t.Errorf("Updater 为 nil 时 enabled 应为 false，实际 %v", got["enabled"])
	}
	if got["repo"] != "kosje/agnes-hub-go-DSM" {
		t.Errorf("套件版的 repo 应为 kosje/agnes-hub-go-DSM，实际 %v", got["repo"])
	}
	if got["current_version"] != "1.0.13" {
		t.Errorf("current_version 应为 1.0.13，实际 %v", got["current_version"])
	}

	// 分支二：自更新启用 —— repo 必须跟随自更新仓库，否则链接与「立即更新」
	// 实际会拉取的来源对不上
	upd := updater.New(updater.Config{Repo: "my788525/agnes-hub-go"},
		"1.0.13", "/tmp/agnes-hub-go", nil)
	s2 := &Server{Version: "1.0.13", ReleaseRepo: "kosje/agnes-hub-go-DSM", Updater: upd}
	got2 := s2.updateStatus()
	if got2["enabled"] != true {
		t.Errorf("Updater 非 nil 时 enabled 应为 true，实际 %v", got2["enabled"])
	}
	if got2["repo"] != "my788525/agnes-hub-go" {
		t.Errorf("自更新启用时 repo 应跟随自更新仓库，实际 %v", got2["repo"])
	}

	// 分支三：自更新仓库为空串（未配置）时回落到套件仓库，不能给出空链接
	upd3 := updater.New(updater.Config{}, "1.0.13", "/tmp/agnes-hub-go", nil)
	s3 := &Server{Version: "1.0.13", ReleaseRepo: "kosje/agnes-hub-go-DSM", Updater: upd3}
	if got3 := s3.updateStatus(); got3["repo"] != "kosje/agnes-hub-go-DSM" {
		t.Errorf("自更新仓库为空时应回落到套件仓库，实际 %v", got3["repo"])
	}
}

// TestUpdateStatusSuiteManagedSeparatesCheckFromApply 钉住套件版的两个开关必须分开。
//
// 「能不能应用更新」和「能不能查最新版本」是两件事：
//   - 应用更新要进程内替换二进制，套件版必须禁止（INFO 里登记了 package.tgz 的
//     checksum，换掉二进制会让「实际内容」与「已安装版本」对不上，下次套件中心
//     校验或升级必然冲突）；
//   - 查最新版本是只读的、没有任何副作用，套件版必须保留 —— 否则控制台的
//     「最新版本 / 发布时间」永远只能显示「—」，用户根本不知道有没有新版。
//
// 曾经把这两件事合在一个 enabled 里，导致套件版查不到版本号。
func TestUpdateStatusSuiteManagedSeparatesCheckFromApply(t *testing.T) {
	upd := updater.New(updater.Config{Repo: "kosje/agnes-hub-go-DSM"},
		"1.0.13", "/tmp/agnes-hub-go", nil)

	// 套件版：能查、不能装
	suite := &Server{Version: "1.0.13", ReleaseRepo: "kosje/agnes-hub-go-DSM",
		Updater: upd, SuiteManaged: true}
	got := suite.updateStatus()
	if got["suite_managed"] != true {
		t.Errorf("suite_managed 应为 true，实际 %v", got["suite_managed"])
	}
	if got["enabled"] != false {
		t.Errorf("套件版的 enabled（能否进程内替换二进制）应为 false，实际 %v", got["enabled"])
	}
	if got["checkable"] != true {
		t.Errorf("套件版的 checkable（能否查最新版本）应为 true，实际 %v", got["checkable"])
	}

	// 自更新版：能查、能装
	self := &Server{Version: "1.0.13", ReleaseRepo: "kosje/agnes-hub-go-DSM", Updater: upd}
	got2 := self.updateStatus()
	if got2["suite_managed"] != false || got2["enabled"] != true || got2["checkable"] != true {
		t.Errorf("自更新版应 suite_managed=false / enabled=true / checkable=true，实际 %v", got2)
	}

	// 未配置更新仓库：两个能力都必须为 false，前端才不会一直转圈去联网
	none := &Server{Version: "1.0.13", ReleaseRepo: "kosje/agnes-hub-go-DSM"}
	got3 := none.updateStatus()
	if got3["enabled"] != false || got3["checkable"] != false {
		t.Errorf("未配置更新仓库时 enabled / checkable 都应为 false，实际 %v", got3)
	}
}

// TestApplyUpdateRefusedWhenSuiteManaged 套件版必须拒绝进程内自更新。
//
// 群晖 INFO 里登记了 package.tgz 的 checksum，进程内换掉二进制会让「实际内容」
// 与「已安装版本」对不上，下次套件中心校验或升级必然冲突。
func TestApplyUpdateRefusedWhenSuiteManaged(t *testing.T) {
	upd := updater.New(updater.Config{Repo: "kosje/agnes-hub-go-DSM"},
		"1.0.13", "/tmp/agnes-hub-go", nil)
	store := newTempStore(t)
	s := &Server{Store: store, Version: "1.0.13",
		ReleaseRepo: "kosje/agnes-hub-go-DSM", Updater: upd, SuiteManaged: true}

	// 这个接口要求管理员会话，得带上有效 cookie，否则先被鉴权拦成 401
	req := httptest.NewRequest(http.MethodPost, "/api/update/apply", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: store.SessionToken()})
	rec := httptest.NewRecorder()
	s.apiUpdateApply(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200 + success=false，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, rec.Body.String())
	}
	if out["success"] != false {
		t.Errorf("套件版必须拒绝应用更新，实际 %v", out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "套件中心") {
		t.Errorf("错误文案应引导用户去套件中心，实际 %q", msg)
	}
}
