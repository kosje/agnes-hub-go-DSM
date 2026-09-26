package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败：%v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// model_manifest 的 nil / 空数组语义
//
// 这是本项目最容易被静默破坏的一条契约：
//	nil          = 还没声明 → 用默认清单补齐
//	非 nil 空数组 = 明示不支持该模态 → 保留为空，调度器据此把账号排除出该池
// 一旦被 JSON 往返或 append 技巧抹平，管理端「清空某模态」会静默失效，
// 运行时完全看不出异常，只表现为「配了不生效」。
// ---------------------------------------------------------------------------

func TestModelManifestClonePreservesNilVsEmpty(t *testing.T) {
	original := ModelManifest{
		Text:  []string{"agnes-2.5-flash"},
		Image: []string{}, // 显式空：明示不支持生图
		Video: nil,        // 未声明
	}
	cp := original.Clone()

	if cp.Image == nil || len(cp.Image) != 0 {
		t.Fatalf("显式空数组必须保持「非 nil 且长度 0」，实际 %#v", cp.Image)
	}
	if cp.Video != nil {
		t.Fatalf("未声明必须保持 nil，实际 %#v", cp.Video)
	}
	if len(cp.Text) != 1 || cp.Text[0] != "agnes-2.5-flash" {
		t.Fatalf("文本清单应被原样复制，实际 %#v", cp.Text)
	}

	// 深拷贝：改副本不得影响原件
	cp.Text[0] = "agnes-3.0-flash"
	if original.Text[0] != "agnes-2.5-flash" {
		t.Fatal("Clone 必须是深拷贝，否则控制台一改就会污染正在被调度读取的结构")
	}
}

func TestAddAccountKeepsExplicitEmptyManifest(t *testing.T) {
	s := newTestStore(t)
	a := s.AddAccount("文本专用", "sk-1", "free", "",
		&ModelManifest{Text: []string{"agnes-2.5-flash"}, Image: []string{}, Video: []string{}})

	if a.ModelManifest.Image == nil {
		t.Fatal("显式清空的 image 清单不得被默认值还原（这会让「按模态分工」静默失效）")
	}
	if len(a.ModelManifest.Image) != 0 || len(a.ModelManifest.Video) != 0 {
		t.Fatalf("image/video 清单应保持为空，实际 image=%v video=%v",
			a.ModelManifest.Image, a.ModelManifest.Video)
	}
	if len(a.ModelManifest.Text) != 1 {
		t.Fatalf("text 清单应被保留，实际 %v", a.ModelManifest.Text)
	}
}

func TestAddAccountNilManifestUsesDefaultAndSurvivesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	s.AddAccount("全部可用", "sk-1", "free", "", nil)

	// 重新载入：显式空数组与 nil 的差异必须跨进程存活
	empty := s.AddAccount("只要文本", "sk-2", "free", "",
		&ModelManifest{Text: []string{"agnes-2.5-flash"}, Image: []string{}, Video: []string{}})
	reloaded, err := NewStore(dir)
	if err != nil {
		t.Fatalf("重新载入失败：%v", err)
	}
	if got := reloaded.AccountByID(empty.ID); got == nil {
		t.Fatal("账号应被持久化")
	} else {
		if got.ModelManifest.Image == nil {
			t.Fatal("显式空数组经 JSON 往返后仍必须是空数组，不能被还原为默认清单")
		}
		if got.ModelManifest.Video == nil {
			t.Fatal("显式空数组经 JSON 往返后仍必须是空数组")
		}
	}
	if got := reloaded.AccountsSnapshot(); len(got) == 0 {
		t.Fatal("账号池应非空")
	}
}

func TestNormalizeAccountFixesInvalidAccessType(t *testing.T) {
	s := newTestStore(t)
	a := s.AddAccount("未知档位", "sk-1", "no-such-tier", "", nil)
	if a.AccessType != "free" {
		t.Fatalf("未知档位应回落 free，实际 %q", a.AccessType)
	}
	if len(a.ClassesEnabled) != len(PoolClasses) {
		t.Fatalf("未指定池时应默认启用全部池，实际 %v", a.ClassesEnabled)
	}
	if a.MaxConcurrency != 8 || a.LearnedFactor != 1 || a.PoolFactors == nil {
		t.Fatalf("默认值不正确：%+v", a)
	}
	if a.BaseURL != DefaultBaseURL {
		t.Fatalf("空 base_url 应用官方默认值，实际 %q", a.BaseURL)
	}
}

func TestAccountCloneIsDeepCopy(t *testing.T) {
	s := newTestStore(t)
	a := s.AddAccount("账号", "sk-1", "free", "", nil)
	a.RPMOverrides["text"] = 22
	a.PoolFactors["video"] = 0.5

	cp := a.Clone()
	cp.RPMOverrides["text"] = 999
	cp.PoolFactors["video"] = 0.1
	cp.ClassesEnabled = append([]string(nil), "text")

	if a.RPMOverrides["text"] != 22 {
		t.Error("RPMOverrides 必须是深拷贝")
	}
	if a.PoolFactors["video"] != 0.5 {
		t.Error("PoolFactors 必须是深拷贝")
	}
	if len(a.ClassesEnabled) == 1 {
		t.Error("ClassesEnabled 必须是深拷贝")
	}
}

// ---------------------------------------------------------------------------
// RPM 查表
// ---------------------------------------------------------------------------

func TestRPMForOverrideBeatsTierTable(t *testing.T) {
	s := newTestStore(t)
	a := s.AddAccount("账号", "sk-1", "free", "", nil)

	if got := s.RPMFor(a, "text"); got != 20 {
		t.Fatalf("free 档文本池基线应为 20，实际 %v", got)
	}
	if got := s.RPMFor(a, "video"); got != 1 {
		t.Fatalf("free 档视频池基线应为 1，实际 %v", got)
	}
	a.RPMOverrides["text"] = 22
	if got := s.RPMFor(a, "text"); got != 22 {
		t.Fatalf("账号级覆盖应优先，实际 %v", got)
	}
	if got := s.RPMFor(a, "text"); s.RPMFor(a, "text") != 22 {
		t.Fatalf("重复读取应稳定，实际 %v", got)
	}
}

func TestRPMTableCoversEveryPool(t *testing.T) {
	// 任何一个池缺项都会让限流静默回落到 1 RPM（比实际额度严得多），
	// 表现为「莫名变慢」，排查成本极高。
	for _, tier := range AccessTypes {
		table, ok := RPMTable[tier]
		if !ok {
			t.Fatalf("档位 %s 缺少限流表", tier)
		}
		for _, cls := range PoolClasses {
			if v, ok := table[cls]; !ok || v <= 0 {
				t.Errorf("档位 %s 的 %s 池缺少有效 RPM", tier, cls)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 落盘健壮性
// ---------------------------------------------------------------------------

func TestCorruptConfigDoesNotBreakStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{ this is not json"), 0o644); err != nil {
		t.Fatalf("写入坏文件失败：%v", err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("坏配置文件不应让服务起不来：%v", err)
	}
	if s.Settings.SafetyFactor != DefaultSettings().SafetyFactor {
		t.Error("坏文件应回落默认设置")
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json.broken")); err != nil {
		t.Errorf("坏文件应被备份为 .broken 以便排查，实际 %v", err)
	}
}

func TestAtomicWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("初始化失败：%v", err)
	}
	s.AddAccount("账号", "sk-1", "free", "", nil)
	s.SaveSettings()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败：%v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("原子写不应残留临时文件：%s", e.Name())
		}
	}
}

func TestSettingsSnapshotIsDeepCopy(t *testing.T) {
	s := newTestStore(t)
	snap := s.SettingsSnapshot()
	snap.AutoIntent.PreferredModels["text"] = []string{"hacked"}
	snap.ModelManifestDefault.Text[0] = "hacked"
	snap.ModelAliases["x"] = "y"

	live := s.SettingsSnapshot()
	if live.AutoIntent.PreferredModels["text"][0] == "hacked" {
		t.Error("PreferredModels 必须是深拷贝")
	}
	if live.ModelManifestDefault.Text[0] == "hacked" {
		t.Error("ModelManifestDefault 必须是深拷贝")
	}
	if _, ok := live.ModelAliases["x"]; ok {
		t.Error("ModelAliases 必须是深拷贝")
	}
}

func TestNormalizeSettingsFillsMissingFields(t *testing.T) {
	// 旧版本配置文件缺字段时，JSON 反序列化会留零值 —— 零值的 PacingWindowSec
	// 会让节拍间隔变成 0，限流器直接失效。
	var v Settings
	normalizeSettings(&v)
	d := DefaultSettings()
	if v.SafetyFactor != d.SafetyFactor || v.PacingWindowSec != d.PacingWindowSec {
		t.Errorf("安全系数/节拍窗口应被补齐：%+v", v)
	}
	if v.QueueMaxWaitMS != d.QueueMaxWaitMS || v.QueueMaxSize != d.QueueMaxSize {
		t.Errorf("排队参数应被补齐：%+v", v)
	}
	if v.ModelAliases == nil || v.AutoIntent.PreferredModels == nil {
		t.Error("map 字段必须非 nil，否则运行期写入会 panic")
	}
	if len(v.ModelManifestDefault.Text) == 0 {
		t.Error("默认模型清单应被补齐")
	}
}

// TestMigrateClientMediaURL 一次性迁移：把被下拉框默认值钉死的 upstream 交回 auto。
//
// 背景：1.0.18 及以前，控制台「客户端媒体地址」下拉框只有「上游地址」/「本机地址」
// 两项且默认选中上游，而它**每次保存设置页都会把当前选中项写回配置** —— 于是只要
// 动过一次设置页（哪怕只想改图片保留天数），这个字段就被钉死成 upstream。
// 用户明明填了「对外访问地址」，客户端拿到的却还是上游 CDN 地址。
func TestMigrateClientMediaURL(t *testing.T) {
	cases := []struct {
		name      string
		in        Settings
		wantMedia string
		wantVer   int
	}{
		{"upstream + 已配对外地址 → 交回 auto", Settings{ClientMediaURL: "upstream", PublicBaseURL: "https://a.tn:52325"}, "", settingsVersion},
		{"upstream 但没配对外地址 → 保持不动", Settings{ClientMediaURL: "upstream"}, "upstream", settingsVersion},
		{"local 不受影响", Settings{ClientMediaURL: "local", PublicBaseURL: "https://a.tn:52325"}, "local", settingsVersion},
		{"已迁过的不再动（版本号已是最新）", Settings{SettingsVersion: settingsVersion, ClientMediaURL: "upstream", PublicBaseURL: "https://a.tn:52325"}, "upstream", settingsVersion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := c.in
			normalizeSettings(&v)
			if v.ClientMediaURL != c.wantMedia {
				t.Errorf("client_media_url 应为 %q，实际 %q", c.wantMedia, v.ClientMediaURL)
			}
			if v.SettingsVersion != c.wantVer {
				t.Errorf("settings_version 应为 %d，实际 %d", c.wantVer, v.SettingsVersion)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 下游密钥
// ---------------------------------------------------------------------------

func TestKeyLifecycleQuotaAndCharge(t *testing.T) {
	s := newTestStore(t)
	k := s.AddKey("测试密钥", []string{"text"}, 2, 0, "")
	if k.Key == "" || len(k.Key) < 10 {
		t.Fatalf("应生成有足够熵的密钥，实际 %q", k.Key)
	}
	if got := s.KeyByValue(k.Key); got == nil || got.Name != "测试密钥" {
		t.Fatal("应能按密钥值查到")
	}
	if s.KeyByValue("sk-not-exist") != nil {
		t.Fatal("不存在的密钥应返回 nil")
	}

	if reason := s.QuotaExceeded(k); reason != "" {
		t.Fatalf("尚未使用不应超限，实际 %q", reason)
	}
	s.ChargeKey(k.Key)
	s.ChargeKey(k.Key)
	if reason := s.QuotaExceeded(k); reason == "" {
		t.Fatal("已达每日额度上限应报超限")
	}

	// 跨天滚动
	s.MutateKey(k.Key, func(kk *DownstreamKey) bool {
		kk.UsedDate = "2000-01-01"
		return true
	})
	if reason := s.QuotaExceeded(s.KeyByValue(k.Key)); reason != "" {
		t.Fatalf("跨天后每日用量应重新起算，实际 %q", reason)
	}
}

func TestTotalQuotaIsEnforced(t *testing.T) {
	s := newTestStore(t)
	k := s.AddKey("总额度密钥", nil, 0, 1, "")
	s.ChargeKey(k.Key)
	if reason := s.QuotaExceeded(s.KeyByValue(k.Key)); reason == "" {
		t.Fatal("已达总额度上限应报超限")
	}
}

func TestKeyAllowsClass(t *testing.T) {
	if KeyAllowsClass(nil, "text") {
		t.Error("nil 密钥不得放行")
	}
	if !KeyAllowsClass(&DownstreamKey{Classes: nil}, "text") {
		t.Error("空 Classes 视为不限池")
	}
	if !KeyAllowsClass(&DownstreamKey{Classes: []string{"*"}}, "video") {
		t.Error("通配符应放行任意池")
	}
	if KeyAllowsClass(&DownstreamKey{Classes: []string{"text"}}, "video") {
		t.Error("未授权的池必须拒绝")
	}
}

// ---------------------------------------------------------------------------
// 粘性绑定与视频任务
// ---------------------------------------------------------------------------

func TestBindingsGCExpiresStaleEntries(t *testing.T) {
	s := newTestStore(t)
	s.UpdateSettings(func(st *Settings) { st.SessionTTLHours = 1 })

	s.Bind("ses:fresh", "acc_1")
	s.mu.Lock()
	s.Bindings["ses:stale"] = Binding{
		AccountID: "acc_2",
		Updated:   float64(time.Now().Add(-2*time.Hour).UnixNano()) / 1e9,
	}
	s.mu.Unlock()

	// 任意一次写入都会顺带清理过期绑定
	s.Bind("ses:another", "acc_3")
	if _, ok := s.BindingsGet("ses:stale"); ok {
		t.Error("超过 TTL 的绑定应被清理，否则文件会无限增长")
	}
	if _, ok := s.BindingsGet("ses:fresh"); !ok {
		t.Error("未过期的绑定不得被误删")
	}
}

func TestBindEmptySessionKeyIsNoop(t *testing.T) {
	s := newTestStore(t)
	s.Bind("", "acc_1")
	if len(s.BindingsSnapshot()) != 0 {
		t.Error("空粘性键不应写入绑定（关闭粘性时全程走这个分支）")
	}
}

func TestVideoJobMapping(t *testing.T) {
	s := newTestStore(t)
	s.PutJob(&VideoJob{JobID: "job_1", VideoID: "vid_1", Model: "agnes-video-2.5-flash",
		AccountID: "acc_1", Status: "running"})

	got, ok := s.JobByID("job_1")
	if !ok || got.VideoID != "vid_1" {
		t.Fatalf("应按 job_id 取回任务，实际 %+v", got)
	}
	// 下游只拿到 job_id，必须能反查到 video_id 与承载账号
	// （否则换账号后无法再向正确的上游端点轮询）
	if got, ok := s.JobByVideoID("vid_1"); !ok || got.JobID != "job_1" {
		t.Fatalf("应能按 video_id 反查，实际 %+v", got)
	}
	if len(s.JobsSnapshot()) != 1 {
		t.Error("任务列表应包含该任务")
	}
}

func TestUsageLogAppendAndTail(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		s.AppendUsage(map[string]any{"i": i})
	}
	got := s.TailUsage(3)
	if len(got) != 3 {
		t.Fatalf("应取回最近 3 条，实际 %d", len(got))
	}
	// 倒序：最新的在最前
	if v, _ := got[0]["i"].(float64); v != 4 {
		t.Errorf("日志应倒序返回，实际首条 i=%v", got[0]["i"])
	}
}

// ---------------------------------------------------------------------------
// 鉴权
// ---------------------------------------------------------------------------

func TestPasswordHashAndSessionToken(t *testing.T) {
	s := newTestStore(t)
	if !s.VerifyPassword("admin123") {
		t.Fatal("初始口令应为 admin123")
	}
	if s.VerifyPassword("wrong") {
		t.Fatal("错误口令必须拒绝")
	}
	before := s.SessionToken()

	if err := s.SetPassword("new-secret"); err != nil {
		t.Fatalf("改密失败：%v", err)
	}
	if !s.VerifyPassword("new-secret") || s.VerifyPassword("admin123") {
		t.Fatal("改密后应只认新口令")
	}
	if s.SessionToken() == before {
		t.Fatal("改密后会话令牌必须变化，否则旧会话无法失效")
	}
	if s.Settings.MustChangePassword {
		t.Error("改密后不应再要求强制改密")
	}
}

// TestHashPasswordUsesSalt 同口令不同盐必须得到不同散列。
func TestHashPasswordUsesSalt(t *testing.T) {
	if HashPassword("p", "salt-a") == HashPassword("p", "salt-b") {
		t.Fatal("散列必须掺盐，否则相同口令可被彩虹表直接命中")
	}
}

func TestJSONRoundTripKeepsZeroValuedSettingsMeaningful(t *testing.T) {
	// VideoWaitSec=0 表示「提交后立即返回、不阻塞等结果」，是合法配置，
	// 不能被当成「缺字段」而改写。
	s := newTestStore(t)
	s.UpdateSettings(func(st *Settings) { st.AutoIntent.VideoWaitSec = 0 })
	reloaded, err := NewStore(s.Dir)
	if err != nil {
		t.Fatalf("重载失败：%v", err)
	}
	if reloaded.Settings.AutoIntent.VideoWaitSec != 0 {
		t.Errorf("VideoWaitSec=0 应被原样保留，实际 %d", reloaded.Settings.AutoIntent.VideoWaitSec)
	}
	buf, _ := json.Marshal(reloaded.Settings)
	if len(buf) == 0 {
		t.Fatal("设置应可序列化")
	}
}
