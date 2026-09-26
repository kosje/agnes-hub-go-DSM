package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agneshub/internal/config"
)

// ---------------------------------------------------------------------------
// 生成结果的本地落盘与回放（图片 / 视频共用）
//
// 为什么不让前端直接引用上游地址，也不把内容内联进正文：
//
//	上游地址 —— 往往带鉴权、或落在浏览器够不到的 CDN 上，直接写进 Markdown /
//	            <video src> 只会得到一个裂图或一个转不动的播放器（实测如此）。
//	base64   —— 图片能显示，但一张 3 MB 的图转成 base64 要 4 MB，正文里塞一次、
//	            images[] 里再塞一次，响应凭空涨到 8 MB；视频更不可能这么干。
//
// 所以由服务端把内容取回来落盘，正文里只放一个短短的本地地址
// （/api/chat/images|videos/<sha256 前 32 位>.<ext>）。响应小、记录小、能长期回看，
// 而且地址是内容寻址的，同一个文件天然去重。
// ---------------------------------------------------------------------------

// mediaKind 描述一类可落盘的媒体。图片和视频的差别全部收在这里，
// 免得「改图片顺手也改了视频」——两边的体积上限差着三十倍。
type mediaKind struct {
	name      string            // 用于日志与错误文案
	dirName   string            // 数据目录下的子目录名
	route     string            // 对外地址前缀
	maxBytes  int64             // 单个文件上限
	timeout   time.Duration     // 单次回取的总超时
	extByMIME map[string]string // Content-Type → 扩展名
	extByURL  map[string]string // URL 后缀 → 扩展名（可为空）
}

// imageKind 是生图结果。
//
// 6 MB 上限的依据：实测上游生图产出普遍在 1~3 MB，6 MB 足够覆盖 1K~4K，
// 再大的图落盘只会白白吃掉 NAS 空间。
var imageKind = mediaKind{
	name:     "图片",
	dirName:  "images",
	route:    "/api/chat/images/",
	maxBytes: 6 << 20,
	timeout:  30 * time.Second,
	extByMIME: map[string]string{
		"image/png":     ".png",
		"image/jpeg":    ".jpg",
		"image/webp":    ".webp",
		"image/gif":     ".gif",
		"image/bmp":     ".bmp",
		"image/svg+xml": ".svg",
	},
}

// videoKind 是生视频结果。
//
// 与图片的三个关键差别：
//  1. 体积大一个量级（一段 1080P/12 秒约 10~30 MB），上限放到 200 MB；
//  2. 回取慢，超时给到 5 分钟；
//  3. 扩展名优先按 URL 后缀定 —— CDN 对视频常回 application/octet-stream，
//     而 http.DetectContentType 对视频的识别也不如对图片可靠。
var videoKind = mediaKind{
	name:     "视频",
	dirName:  "videos",
	route:    "/api/chat/videos/",
	maxBytes: 200 << 20,
	timeout:  5 * time.Minute,
	extByMIME: map[string]string{
		"video/mp4":        ".mp4",
		"video/webm":       ".webm",
		"video/quicktime":  ".mov",
		"video/x-m4v":      ".m4v",
		"video/x-matroska": ".mkv",
	},
	extByURL: map[string]string{
		".mp4": ".mp4", ".m4v": ".m4v", ".webm": ".webm",
		".mov": ".mov", ".mkv": ".mkv",
	},
}

// mediaFetchClient 专用于回取上游产出。
//
// 刻意与 relay.BuildClient() 分开：那个客户端把 CheckRedirect 设成
// http.ErrUseLastResponse（要把上游的 302 原样透给调用方），而 CDN 地址
// 经常就是 302 到真正的存储节点 —— 用它取内容必然拿不到。
//
// 不设全局 Timeout：视频要下几分钟，全局超时会拦腰截断。改用每类的
// context 超时（见 mediaKind.timeout）。
var mediaFetchClient = &http.Client{}

// mediaDir 返回该类媒体的缓存目录绝对路径。
//
// 放在数据目录下是刻意的：群晖套件的数据目录会被「套件中心 → 卸载保留数据」
// 和 Hyper Backup 一并带走，产出不会散落在系统临时目录里被清理掉。
func (s *Server) mediaDir(k mediaKind) string {
	return filepath.Join(s.Store.Dir, k.dirName)
}

// pickExt 决定落盘用哪个扩展名。
//
// 顺序：URL 后缀 → Content-Type → 内容嗅探。视频必须把 URL 后缀放最前，
// 因为 CDN 给视频的 Content-Type 经常是 octet-stream。
func (k mediaKind) pickExt(rawURL, contentType string, head []byte) string {
	if len(k.extByURL) > 0 {
		if u, err := url.Parse(rawURL); err == nil {
			if ext, ok := k.extByURL[strings.ToLower(filepath.Ext(u.Path))]; ok {
				return ext
			}
		}
	}
	if ext, ok := k.extByMIME[normalizeMIME(contentType)]; ok {
		return ext
	}
	if len(head) > 0 {
		if ext, ok := k.extByMIME[http.DetectContentType(head)]; ok {
			return ext
		}
	}
	return ""
}

// allowsExt 判断扩展名是否属于本类媒体（白名单，用于文件名校验）。
func (k mediaKind) allowsExt(ext string) bool {
	for _, e := range k.extByMIME {
		if e == ext {
			return true
		}
	}
	for _, e := range k.extByURL {
		if e == ext {
			return true
		}
	}
	return false
}

// mimeForExt 是 extByMIME 的反查，用于回放时设置 Content-Type。
func (k mediaKind) mimeForExt(ext string) string {
	for mime, e := range k.extByMIME {
		if e == ext {
			return mime
		}
	}
	return "application/octet-stream"
}

// downloadMedia 把上游产出**流式**写进缓存目录，返回本地地址。
//
// 流式而不是先读进内存：视频动辄几十 MB，在内存吃紧的 NAS 上 io.ReadAll
// 很容易把进程顶爆。这里边下边写临时文件，超限或出错就删掉半成品。
//
// 任何一步失败都返回 ok=false —— 宁可退化成上游地址，也不能让整个响应挂掉。
func (s *Server) downloadMedia(ctx context.Context, k mediaKind, rawURL string) (string, bool) {
	dir := s.mediaDir(k)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}

	fctx, cancel := context.WithTimeout(ctx, k.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", false
	}
	resp, err := mediaFetchClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	// 先读一小段用于嗅探扩展名，再接着往盘上写。
	// ReadFull 在不足 512 字节时会返回 ErrUnexpectedEOF，那属于正常情况，忽略即可。
	head := make([]byte, 512)
	n, _ := io.ReadFull(resp.Body, head)
	head = head[:n]
	ext := k.pickExt(rawURL, resp.Header.Get("Content-Type"), head)
	if ext == "" {
		return "", false
	}

	tmp, err := os.CreateTemp(dir, "dl-*.part")
	if err != nil {
		return "", false
	}
	tmpName := tmp.Name()
	discard := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	// 边写盘边算 sha256，避免为了内容寻址再读一遍几十 MB 的文件。
	hasher := sha256.New()
	sink := io.MultiWriter(tmp, hasher)
	if _, err := sink.Write(head); err != nil {
		discard()
		return "", false
	}
	// 多读 1 字节：读满 limit+1 就说明超限，直接放弃，不用把整个文件下完。
	rest, err := io.Copy(sink, io.LimitReader(resp.Body, k.maxBytes-int64(n)+1))
	total := int64(n) + rest
	if err != nil || total > k.maxBytes || total == 0 {
		discard()
		return "", false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", false
	}

	name := hex.EncodeToString(hasher.Sum(nil)[:16]) + ext
	dest := filepath.Join(dir, name)
	// 已经存在就跳过写入（内容寻址，同名必同内容）。
	if _, err := os.Stat(dest); err == nil {
		_ = os.Remove(tmpName)
		return k.route + name, true
	}
	// rename 是原子的：并发下前端不会拿到一个只写了一半的文件。
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return "", false
	}
	s.pruneMediaCache(k)
	return k.route + name, true
}

// saveMediaBytes 把已经在内存里的内容落盘（上游回 b64_json 时用）。
func (s *Server) saveMediaBytes(k mediaKind, data []byte, mime string) (string, bool) {
	ext := ""
	if e, ok := k.extByMIME[normalizeMIME(mime)]; ok {
		ext = e
	} else if e, ok := k.extByMIME[http.DetectContentType(data)]; ok {
		ext = e
	}
	if ext == "" || len(data) == 0 || int64(len(data)) > k.maxBytes {
		return "", false
	}
	dir := s.mediaDir(k)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:16]) + ext
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err == nil {
		return k.route + name, true
	}
	tmp, err := os.CreateTemp(dir, "dl-*.part")
	if err != nil {
		return "", false
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", false
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return "", false
	}
	s.pruneMediaCache(k)
	return k.route + name, true
}

// surfaceHeader 是网关自家网页表明身份的请求头（见 chat.main.js 的 SURFACE_HEADERS）。
const surfaceHeader = "X-Agnes-Hub-Surface"

// webSurface 判断这次请求是不是来自网关自己的网页。
func webSurface(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(surfaceHeader)), "web")
}

// clientMediaMode 归一化 ClientMediaURL：空串与 "auto" 等价。
func clientMediaMode(settings config.Settings) string {
	switch strings.ToLower(strings.TrimSpace(settings.ClientMediaURL)) {
	case "local":
		return "local"
	case "upstream":
		return "upstream"
	default:
		return "auto"
	}
}

// clientFacingMediaURL 算出「外部 AI 客户端会收到哪个地址」。
//
// 与 callerMediaURL 同一套规则，但**刻意不看这次请求是不是来自网关自己的网页**：
// 自检面板要回答的是「外部客户端收到什么」，而面板本身就是一个网页请求 ——
// 直接复用 callerMediaURL 会被 webSurface 短路成网关地址，于是无论怎么配，
// 面板永远显示「客户端拿到的是网关地址」。1.0.18 的自检面板正是这么自欺欺人的。
func (s *Server) clientFacingMediaURL(settings config.Settings, upstream, local string) string {
	if local == "" {
		return upstream
	}
	switch clientMediaMode(settings) {
	case "local":
		return local
	case "upstream":
		return upstream
	default: // auto
		// 填了「对外访问地址」＝ 运营者明确说过「客户端从这个地址找我」，
		// 那就该给网关地址；没填才退回上游（公网 https，够得到但不离线）。
		if strings.TrimSpace(settings.PublicBaseURL) != "" {
			return local
		}
		return upstream
	}
}

// callerMediaURL 决定把哪个地址回给调用方。
//
// upstream 是上游原始地址，local 是落盘后的本地地址（空 = 没落盘成功，一律退回上游）。
//
// 为什么不一律给本地地址 —— 这是踩过的坑：/api/chat/v1/* 同时服务于两种调用方，
// 而它们对媒体地址的要求正好相反。
//
//	网关自己的网页：与网关同源，本地地址一定可达，也正是落盘的意义所在；
//	外部 AI 客户端：往往在另一个网络位置，甚至跑在 https 页面里。指向 NAS 的
//	  http 地址经常加载不出来 —— 实测客户端会退化成显示图片的 alt 文本
//	  （用户看到的就是一串莫名其妙的提示词）。上游产出地址是公网 https，
//	  实测在客户端里能正常显示。
//
// 所以默认按调用方分流：网页一律本地；客户端交给 clientFacingMediaURL 判
// （auto 时配了「对外访问地址」就给网关地址）。
func (s *Server) callerMediaURL(r *http.Request, settings config.Settings, upstream, local string) string {
	if local == "" {
		return upstream
	}
	if webSurface(r) {
		return local
	}
	return s.clientFacingMediaURL(settings, upstream, local)
}

// publicBaseURL 推导「客户端能访问到的」基地址（形如 http://nas:4142）。
//
// 为什么必须给外部客户端绝对地址：网关自己网页里的 /api/chat/images/xxx.png 是
// 相对地址，靠页面的 origin 解析；而 AI 客户端（WorkBuddy / Cherry Studio …）拿到的
// 是一段 Markdown，没有任何基准可以解析这个相对路径 —— 图片必然加载失败，客户端
// 只能退化成显示 alt 文本（实测用户看到的就是一串莫名其妙的提示词）。
//
// 优先用显式配置；否则按请求头推断 —— 走反向代理时 r.Host 可能是内网地址，
// 所以先看 X-Forwarded-*。
func publicBaseURL(r *http.Request, settings config.Settings) string {
	if s := strings.TrimSpace(settings.PublicBaseURL); s != "" {
		return strings.TrimRight(s, "/")
	}
	if r == nil {
		return ""
	}
	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = strings.ToLower(p)
	}
	return scheme + "://" + host
}

// mediaBaseSource 说明当前基地址是哪来的，便于排查代理配置。
func mediaBaseSource(r *http.Request, settings config.Settings) string {
	if strings.TrimSpace(settings.PublicBaseURL) != "" {
		return "显式配置（对外访问地址）"
	}
	if r != nil && firstHeaderValue(r.Header.Get("X-Forwarded-Host")) != "" {
		return "X-Forwarded-Host 推断"
	}
	return "请求 Host 推断"
}

// apiMediaBase 回显「按当前这次请求推算出的媒体基地址」。
//
// 专为反向代理部署排查而设：从外网访问时这里应该是外网地址（**含端口**），
// 从局域网访问时应该是局域网地址。不一致通常意味着两件事之一：
//  1. 代理没传 X-Forwarded-* —— 网关只能按 Host 推断；
//  2. 代理把 Host/X-Forwarded-Host 设成了 $host（不带端口），
//     网关推断出来会少一段端口 —— 这时必须显式填「对外访问地址」。
func (s *Server) apiMediaBase(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.deny(w)
		return
	}
	settings := s.Store.SettingsSnapshot()
	base := publicBaseURL(r, settings)
	sample := base + imageKind.route + "0123456789abcdef0123456789abcdef.png"
	writeJSON(w, 200, map[string]any{
		"base_url":          base,
		"source":            mediaBaseSource(r, settings),
		"sample_image_url":  sample,
		"client_media_url":  settings.ClientMediaURL,
		"client_media_mode": clientMediaMode(settings),
		// 注意这里传的是 clientFacingMediaURL 而不是 callerMediaURL：
		// 面板自己就是网页请求，用后者会被 webSurface 短路，永远显示「已生效」。
		"url_for_client":    s.clientFacingMediaURL(settings, "https://上游产出地址/x.png", sample),
		"host":              r.Host,
		"x_forwarded_host":  r.Header.Get("X-Forwarded-Host"),
		"x_forwarded_proto": r.Header.Get("X-Forwarded-Proto"),
		"tls":               r.TLS != nil,
	}, nil)
}

// firstHeaderValue 取逗号分隔头里的第一个值（代理链会追加多个）。
func firstHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// localizeMediaURL 把上游地址换成「先落盘、再返回本地地址」。
//
// base 非空时返回绝对地址（外部客户端需要），否则返回相对路径（网关自己的网页够用）。
// 已是本地地址、已是 data URI、非 http(s)、或取回失败时原样返回入参。
func (s *Server) localizeMediaURL(ctx context.Context, k mediaKind, raw, base string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, k.route) || strings.HasPrefix(raw, "data:") {
		return raw
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return raw
	}
	local, ok := s.downloadMedia(ctx, k, raw)
	if !ok {
		return raw
	}
	if base == "" {
		return local
	}
	return base + local
}

// localizeImageURL 把上游图片地址本地化。
func (s *Server) localizeImageURL(ctx context.Context, raw, base string) string {
	return s.localizeMediaURL(ctx, imageKind, raw, base)
}

// localizeVideoURL 把上游视频地址本地化。
//
// 与图片同源，但走 videoKind：上限 200 MB、超时 5 分钟、扩展名优先按 URL 后缀定。
func (s *Server) localizeVideoURL(ctx context.Context, raw, base string) string {
	return s.localizeMediaURL(ctx, videoKind, raw, base)
}

// localizeImageURLs 对生图结果逐条做本地化，返回新切片。
//
// 每条都会**先落盘**（这是「下载到本地」的落点），再按调用方是谁决定回哪个地址
// （见 callerMediaURL）。返回新切片、不改动入参：入参还要按「官方形态」原样回给
// 程序化客户端（那些客户端要的是真实的上游地址，不是我们本地的缓存路径）。
func (s *Server) localizeImageURLs(r *http.Request, items []map[string]any, settings config.Settings) []map[string]any {
	if len(items) == 0 {
		return items
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	base := publicBaseURL(r, settings)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		clone := make(map[string]any, len(item)+1)
		for k, v := range item {
			clone[k] = v
		}
		// 只处理字符串型 url：上游偶尔会回 null 或对象，交给下游按原样处理即可。
		upstream, _ := clone["url"].(string)
		// 上游有时只回 b64_json 不回 url。先落盘，正文里就不必内联那一大串
		// base64 了（ImageContent 对 b64_json 是直接拼 data URI 的）。
		if strings.TrimSpace(upstream) == "" {
			if b64, ok := clone["b64_json"].(string); ok && strings.TrimSpace(b64) != "" {
				if data, err := decodeBase64Loose(b64); err == nil {
					if local, ok := s.saveMediaBytes(imageKind, data, ""); ok {
						clone["url"] = s.callerMediaURL(r, settings, upstream, base+local)
					}
				}
			}
			out = append(out, clone)
			continue
		}
		local := s.localizeImageURL(ctx, upstream, base)
		clone["url"] = s.callerMediaURL(r, settings, upstream, local)
		out = append(out, clone)
	}
	return out
}

// mediaCacheLimits 返回该类缓存的清理阈值（按条数 + 按总字节）。
func (s *Server) mediaCacheLimits(k mediaKind) (keepCount int, keepBytes int64) {
	settings := s.Store.SettingsSnapshot()
	if k.dirName == videoKind.dirName {
		keepCount = settings.VideoMaxCapacity
		if keepCount <= 0 {
			keepCount = 200
		}
		// 视频必须同时卡总字节：一段 1080P 视频 10~30 MB，只卡条数的话
		// 200 条能吃掉好几个 GB。0 表示不限。
		if mb := settings.VideoCacheMaxMB; mb > 0 {
			keepBytes = int64(mb) << 20
		}
		return keepCount, keepBytes
	}
	keepCount = settings.ImageMaxCapacity
	if keepCount <= 0 {
		keepCount = 500
	}
	return keepCount, 0
}

// pruneMediaCache 控制缓存目录的体积。
//
// 产出是「生成一次、看几次」的东西，但落盘后不会自己消失 —— 不加限制的话
// 一个长期运行的 NAS 会被慢慢填满。策略：按修改时间从新到旧保留，
// 同时受「条数上限」和「总字节上限」两个约束，超出即删。
func (s *Server) pruneMediaCache(k mediaKind) {
	keepCount, keepBytes := s.mediaCacheLimits(k)
	dir := s.mediaDir(k)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		name string
		mod  time.Time
		size int64
	}
	files := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// 正在下载中的临时文件由下载方自己负责清理，不要在这里误删。
		if strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{e.Name(), info.ModTime(), info.Size()})
	}
	if len(files) <= keepCount && keepBytes <= 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })

	var total int64
	for i, f := range files {
		total += f.size
		if i < keepCount && (keepBytes <= 0 || total <= keepBytes) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}

// validMediaName 校验文件名形状：32 位十六进制 + 本类媒体的白名单扩展名。
//
// 这是路径穿越的唯一一道闸门 —— 文件名会被拼进 filepath.Join，
// 放任 ../ 进来就能读到数据目录里的 settings.json（里面存着 API Key 与口令哈希）。
func validMediaName(k mediaKind, name string) bool {
	base, ext, ok := strings.Cut(name, ".")
	if !ok || len(base) != 32 {
		return false
	}
	for i := 0; i < len(base); i++ {
		c := base[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return k.allowsExt("." + ext)
}

// handleChatMedia 回放本地落盘的产出。
//
// 刻意**不做会话鉴权**：文件名是内容的 sha256 前 16 字节（128 位），既不可枚举
// 也不可猜测 —— 这是一个 capability URL，与 S3 预签名链接同一类做法。
//
// 之所以必须放开：外部 AI 客户端拿到的只是正文里的一段地址，它没有、也不可能
// 带上网关的会话 cookie；一旦要求登录，客户端里就永远是一张裂图（实测如此）。
// 这与 /api/chat/v1/* 那批生成接口的信任模型是一致的 —— 那些接口本来就免鉴权，
// 而且危害更大（能直接消耗账号配额），媒体回放只是读一份已经生成好的文件。
//
// 用 http.ServeContent 而不是自己 io.Copy：它自带 Range 支持，
// 视频才能拖动进度条、断点续传。
func (s *Server) handleChatMedia(k mediaKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !validMediaName(k, name) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		f, err := os.Open(filepath.Join(s.mediaDir(k), name))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// 内容寻址 → 地址与内容一一对应，可以放心长缓存。
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", k.mimeForExt(filepath.Ext(name)))
		// ?download=1 让浏览器直接存盘而不是内联打开 —— 客户端回复里给出的
		// 「下载地址」就带这个参数。图片本身仍用不带参数的地址内联渲染，
		// 两者读的是同一个文件，只是响应头不同。
		//
		// 文件名换成可读的形式：地址里那串 32 位十六进制是内容哈希，存下来
		// 一文件夹 `9f2c…e1.png` 谁也认不出。name 已过 validMediaName，
		// 只可能由 [0-9a-f] 和允许的后缀组成，塞进响应头是安全的。
		if isDownloadRequest(r) {
			base, ext, _ := strings.Cut(name, ".")
			w.Header().Set("Content-Disposition",
				"attachment; filename=\"agnes-"+base[:8]+"."+ext+"\"")
		}
		http.ServeContent(w, r, name, info.ModTime(), f)
	}
}

// isDownloadRequest 判断这次请求是不是要「直接下载」。
//
// 用 ?download=1 而不是另开一条路径，是为了让同一个文件既能内联渲染又能下载，
// 且两边缓存互不干扰（URL 不同即缓存键不同）。
func isDownloadRequest(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("download"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// normalizeMIME 去掉 Content-Type 里的参数（charset 等）并统一小写。
func normalizeMIME(ct string) string {
	ct = strings.TrimSpace(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.ToLower(ct)
}

// decodeBase64Loose 解上游的 b64_json。
//
// 上游对 base64 的写法不统一：有的带 data URI 前缀，有的用 URL-safe 字母表，
// 有的省掉 '=' 填充。这里逐个试，都不行才算失败。
func decodeBase64Loose(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ";base64,"); i >= 0 {
		s = s[i+len(";base64,"):]
	}
	s = strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(s)

	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	if data, err := base64.URLEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
