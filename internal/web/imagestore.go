package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 生图结果的本地落盘与回放
//
// 为什么不让前端直接引用上游地址，也不把图片 base64 内联进正文：
//
//	上游地址   —— 往往带鉴权、或落在浏览器够不到的 CDN 上，直接写进 Markdown
//	             只会渲染成一张裂图（实测就是如此）。
//	base64 内联 —— 能显示，但一张 3 MB 的图转成 base64 要 4 MB，正文里塞一次、
//	             images[] 里再塞一次，响应凭空涨到 8 MB；聊天记录（全 JSON 存储）
//	             也会被同一条记录撑爆。
//
// 所以由服务端把图片取回来落盘，正文里只放一个短短的本地地址
// （/api/chat/images/<sha256 前 32 位>.<ext>）。响应小、记录小、能长期回看，
// 而且地址是内容寻址的，同一个文件天然去重。
// ---------------------------------------------------------------------------

// imageCacheDirName 是图片缓存目录名（相对 Store.Dir，即数据目录）。
//
// 放在数据目录下是刻意的：群晖套件的数据目录会被「套件中心 → 卸载保留数据」
// 和 Hyper Backup 一并带走，图片不会散落在系统临时目录里被清理掉。
const imageCacheDirName = "images"

// imageRoutePrefix 是本地图片对外的地址前缀。
const imageRoutePrefix = "/api/chat/images/"

// maxFetchedImageBytes 是服务端回取图片的大小上限。
//
// 超过就退回原始 URL：一张几十 MB 的图落盘会白白吃掉 NAS 空间，
// 得不偿失。实测上游生图产出普遍在 1~3 MB，6 MB 足够覆盖 1K~4K。
const maxFetchedImageBytes = 6 << 20

// imageFetchClient 专用于回取上游图片。
//
// 刻意与 relay.BuildClient() 分开：那个客户端把 CheckRedirect 设成
// http.ErrUseLastResponse（要把上游的 302 原样透给调用方），而 CDN 的图片地址
// 经常就是 302 到真正的存储节点 —— 用它取图必然拿不到内容。
var imageFetchClient = &http.Client{Timeout: 30 * time.Second}

// imageExtByMIME 把 MIME 映射成扩展名。
//
// 必须带上正确的扩展名：群晖的 nginx 按扩展名决定 Content-Type，
// 给 .bin 会让浏览器把图片当成下载而不是渲染。
var imageExtByMIME = map[string]string{
	"image/png":     ".png",
	"image/jpeg":    ".jpg",
	"image/webp":    ".webp",
	"image/gif":     ".gif",
	"image/bmp":     ".bmp",
	"image/svg+xml": ".svg",
}

// imageCacheDir 返回图片缓存目录的绝对路径。
func (s *Server) imageCacheDir() string {
	return filepath.Join(s.Store.Dir, imageCacheDirName)
}

// fetchImageBytes 把上游图片拉回内存。
//
// 任何一步失败都返回 ok=false —— 宁可退化成上游地址，也不能让整个响应挂掉。
func (s *Server) fetchImageBytes(ctx context.Context, raw string) ([]byte, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", false
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(fctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", false
	}
	resp, err := imageFetchClient.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	// 多读 1 字节：读满 limit+1 就说明超限，直接放弃，不用把整张图读完。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchedImageBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxFetchedImageBytes {
		return nil, "", false
	}

	mime := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if _, ok := imageExtByMIME[mime]; !ok {
		// 上游常常把 Content-Type 写成 application/octet-stream，
		// 用魔数兜底，避免存成 .bin 导致浏览器不渲染。
		mime = http.DetectContentType(data)
	}
	if _, ok := imageExtByMIME[mime]; !ok {
		return nil, "", false
	}
	return data, mime, true
}

// saveImageLocal 把图片写进本地缓存目录，返回可对外访问的本地地址。
//
// 文件名是内容的 sha256 前 16 字节 —— 内容寻址带来三个好处：
//  1. 同一张图重复生成只落一份盘；
//  2. 地址与内容一一对应，可以放心发 immutable 缓存头；
//  3. 文件名不可猜（128 位），即使路由不做鉴权也枚举不出别人的图。
func (s *Server) saveImageLocal(data []byte, mime string) (string, bool) {
	dir := s.imageCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:16]) + imageExtByMIME[mime]
	dest := filepath.Join(dir, name)

	// 已经存在就跳过写入（内容寻址，同名必同内容）。
	if _, err := os.Stat(dest); err == nil {
		return imageRoutePrefix + name, true
	}

	// 先写临时文件再 rename：避免并发下前端拿到一个只写了一半的文件。
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return "", false
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
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
	s.pruneImageCache()
	return imageRoutePrefix + name, true
}

// localizeImageURL 把上游图片地址换成「先落盘、再返回本地地址」。
//
// 已是本地地址、或取回失败时原样返回入参。
func (s *Server) localizeImageURL(ctx context.Context, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, imageRoutePrefix) || strings.HasPrefix(raw, "data:") {
		return raw
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return raw
	}
	data, mime, ok := s.fetchImageBytes(ctx, raw)
	if !ok {
		return raw
	}
	local, ok := s.saveImageLocal(data, mime)
	if !ok {
		return raw
	}
	return local
}

// localizeImageURLs 对生图结果逐条做本地化，返回新切片。
//
// 返回新切片、不改动入参：入参还要按「官方形态」原样回给程序化客户端
// （那些客户端要的是真实的上游地址，不是我们本地的缓存路径）。
func (s *Server) localizeImageURLs(ctx context.Context, items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return items
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		clone := make(map[string]any, len(item)+1)
		for k, v := range item {
			clone[k] = v
		}
		// 只处理字符串型 url：上游偶尔会回 null 或对象，交给下游按原样处理即可。
		if u, ok := clone["url"].(string); ok {
			clone["url"] = s.localizeImageURL(ctx, u)
		}
		// 上游有时只回 b64_json 不回 url。落盘一份本地地址，正文里就不必内联
		// 那一大串 base64 了（ImageContent 对 b64_json 是直接拼 data URI 的）。
		if cur, _ := clone["url"].(string); cur == "" {
			if b64, ok := clone["b64_json"].(string); ok && strings.TrimSpace(b64) != "" {
				if data, err := decodeBase64Loose(b64); err == nil && len(data) > 0 {
					mime := http.DetectContentType(data)
					if _, known := imageExtByMIME[mime]; known {
						if local, ok := s.saveImageLocal(data, mime); ok {
							clone["url"] = local
						}
					}
				}
			}
		}
		out = append(out, clone)
	}
	return out
}

// pruneImageCache 控制缓存目录的体积：只保留最新的 keep 个文件。
//
// 图片是「生成一次、看一眼」的东西，但落盘后不会自己消失 —— 不加限制的话
// 一个长期运行的 NAS 会被慢慢填满。容量沿用设置里的 ImageMaxCapacity。
func (s *Server) pruneImageCache() {
	keep := s.Store.SettingsSnapshot().ImageMaxCapacity
	if keep <= 0 {
		keep = 500
	}
	dir := s.imageCacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		name string
		mod  time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{e.Name(), info.ModTime()})
	}
	if len(files) <= keep {
		return
	}
	// 新的在前，超出的从最旧的开始删。
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[keep:] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}

// validImageName 校验文件名形状：32 位十六进制 + 白名单扩展名。
//
// 这是路径穿越的唯一一道闸门 —— 文件名会被拼进 filepath.Join，
// 放任 ../ 进来就能读到数据目录里的 settings.json（里面存着 API Key 与口令哈希）。
func validImageName(name string) bool {
	if len(name) != 32+4 && len(name) != 32+5 { // .png/.jpg 是 4，.webp 是 5
		return false
	}
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
	switch "." + ext {
	case ".png", ".jpg", ".webp", ".gif", ".bmp", ".svg":
		return true
	}
	return false
}

// handleChatImage 回放本地落盘的生图结果。
//
// 需要对话页或管理员会话 —— 文件名虽然不可猜，但把图片接口完全敞开
// 会让任何能访问到端口的人都拿到用户生成过的图。
func (s *Server) handleChatImage(w http.ResponseWriter, r *http.Request) {
	if !s.authedOrChat(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	name := r.PathValue("name")
	if !validImageName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(s.imageCacheDir(), name)
	f, err := os.Open(path)
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
	w.Header().Set("Content-Type", mimeByImageExt(filepath.Ext(name)))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// mimeByImageExt 是 imageExtByMIME 的反查。
func mimeByImageExt(ext string) string {
	for mime, e := range imageExtByMIME {
		if e == ext {
			return mime
		}
	}
	return "application/octet-stream"
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
