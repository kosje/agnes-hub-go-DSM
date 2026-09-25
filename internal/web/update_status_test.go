package web

import (
	"testing"

	"agneshub/internal/updater"
)

// 控制台「版本与自更新」卡片要靠 updateStatus 里的 repo 字段拼「查看全部版本」的跳转地址。
// 套件版（自更新被禁用）以前会漏掉这个字段，前端就退化成写死的上游地址 —— 用户点进去
// 看到的是上游仓库，那里根本没有 SPK。这里把两条分支都钉住。
func TestUpdateStatusRepo(t *testing.T) {
	// 分支一：套件版 —— 自更新未启用，repo 必须回落到 main 注入的套件仓库
	s := &Server{Version: "1.0.11", ReleaseRepo: "kosje/agnes-hub-go-DSM"}
	got := s.updateStatus()
	if got["enabled"] != false {
		t.Errorf("Updater 为 nil 时 enabled 应为 false，实际 %v", got["enabled"])
	}
	if got["repo"] != "kosje/agnes-hub-go-DSM" {
		t.Errorf("套件版的 repo 应为 kosje/agnes-hub-go-DSM，实际 %v", got["repo"])
	}
	if got["current_version"] != "1.0.11" {
		t.Errorf("current_version 应为 1.0.11，实际 %v", got["current_version"])
	}

	// 分支二：自更新启用 —— repo 必须跟随自更新仓库，否则链接与「立即更新」
	// 实际会拉取的来源对不上
	upd := updater.New(updater.Config{Repo: "my788525/agnes-hub-go"},
		"1.0.11", "/tmp/agnes-hub-go", nil)
	s2 := &Server{Version: "1.0.11", ReleaseRepo: "kosje/agnes-hub-go-DSM", Updater: upd}
	got2 := s2.updateStatus()
	if got2["enabled"] != true {
		t.Errorf("Updater 非 nil 时 enabled 应为 true，实际 %v", got2["enabled"])
	}
	if got2["repo"] != "my788525/agnes-hub-go" {
		t.Errorf("自更新启用时 repo 应跟随自更新仓库，实际 %v", got2["repo"])
	}

	// 分支三：自更新仓库为空串（未配置）时回落到套件仓库，不能给出空链接
	upd3 := updater.New(updater.Config{}, "1.0.11", "/tmp/agnes-hub-go", nil)
	s3 := &Server{Version: "1.0.11", ReleaseRepo: "kosje/agnes-hub-go-DSM", Updater: upd3}
	if got3 := s3.updateStatus(); got3["repo"] != "kosje/agnes-hub-go-DSM" {
		t.Errorf("自更新仓库为空时应回落到套件仓库，实际 %v", got3["repo"])
	}
}
