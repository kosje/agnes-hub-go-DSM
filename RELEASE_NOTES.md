基于 [my788525/agnes-hub-go](https://github.com/my788525/agnes-hub-go) **v1.0.11** 构建的群晖 DSM 套件（本套件版自身版本号为 `1.0.12`）。

### 本次更新（1.0.12-0001）

1. **修复「生图 / 生视频」整条链路打不通**（本次最重要的修复，共四处缺陷）：

   - **上游路径带错前缀**：控制台的生图路由是 `/api/chat/v1/images/generations`，转发给上游时没有摘掉 `/api/chat`，实际请求打到了 `{站点}/api/chat/v1/images/generations` —— 上游并不存在这个路径，实测会被边缘 WAF 拦成一张 HTML 错误页（就是界面上那一大坨 `Attention Required! | Cloudflare`）。
   - **`agnes-auto` 不被识别为 auto**：对话页把模型选成 `agnes-auto` 后发「画一幅山水画」也不行。`agnes-auto` 是设置里的 `AutoModelName`，但代码里的 auto 判定只认内置的 `auto` / `auto-all` / `auto*` 前缀，**不认识 `agnes-auto`**，于是请求被当成「显式模型」原样透传，内容判定 → 生图这条链路整条被跳过。
   - **提示词被读空**：对话入口把请求体交给媒体代理时又读了一遍（`readBody` 是直接消费 `r.Body` 的），第二次只会拿到空 map，上游只回「prompt 不能为空」。
   - **响应形态没有回译**：对话入口拿到的上游响应是 images / videos 结构（没有 `choices`），而界面按 chat 的 `choices[0].message.content` 取值，**图其实已经生成好了，界面却显示「（无内容返回）」**。

   以上四处已全部修复，并补了 4 个回归测试钉住行为，其中一条是端到端断言（提示词必须真的到达上游、响应必须是 chat 形态）。

2. **控制台品牌改为 Agnes Hub**：

   - 页面标题、侧边栏与登录页的「baiPiao-hub 控制台」→「**Agnes Hub 控制台**」；
   - 副标题「白嫖 Hub 聚合网关」→「Agnes Hub 聚合网关」；
   - 添加账号的示例名与批量导入示例「白嫖-1 / 白嫖-2」→「账号-1 / 账号-2」。

   这些改动都只是展示文本（标题 + `placeholder`），不涉及任何取值逻辑，账号名照常按你输入的内容保存。

3. **修复网页顶栏图标升级后不更新**：

   - `/logo.png` 原先带 `Cache-Control: public, max-age=86400`（缓存一整天），而 logo 是 `go:embed` 进二进制的、URL 恒定不变 —— 换图标后浏览器会一直用缓存里的旧图。实测从 `1.0.11-0001` 升到 `0003` 后，网页顶栏仍是老图标。
   - 改为 `no-cache` + 内容指纹 `ETag`（内容没变就回 304），并让前端引用地址带上指纹（`/logo.png?v=<指纹>`）。指纹一变 URL 就变，缓存必然击穿，以后换图标不用再等缓存过期。

4. **上游返回 HTML 时不再把整页刷到界面上**：

   - 上游被 WAF / 网关拦截时回的是几万字符的 HTML 页，原先会原样塞进错误信息，把控制台和对话页整个刷满；现在压成一句可读摘要，并点明「这是拦截页，不是业务接口的响应」。

---

### 推荐：添加群晖套件源（自动检查更新）

1. **套件中心** → **设置** → **常规** → **信任层级** 选择「**任何发行者**」；
2. **套件来源** → **新增**：
   - **x86_64 平台**（Intel / AMD 机型）：`https://kosje.github.io/agnes-hub-go-DSM/catalog.json`
   - **armv8 平台**（ARM64 机型，如 DS223/DS423 系列）：`https://kosje.github.io/agnes-hub-go-DSM/catalog-armv8.json`
3. 切换到套件中心「**社群**」标签页即可直接安装或一键更新。

---

### 手动安装下载

- **x86_64 机型**：[agnes-hub-x86_64-1.0.12-0001.spk](https://kosje.github.io/agnes-hub-go-DSM/agnes-hub-x86_64-1.0.12-0001.spk)
- **armv8 机型**：[agnes-hub-armv8-1.0.12-0001.spk](https://kosje.github.io/agnes-hub-go-DSM/agnes-hub-armv8-1.0.12-0001.spk)

---

### 升级后图标 / 页面还是旧的？

1. 在套件中心确认版本已是 `1.0.12-0001`；
2. **强制刷新浏览器**（Windows `Ctrl+F5` / macOS `Cmd+Shift+R`），或换浏览器 / 无痕窗口验证；
3. 顶栏 logo 的缓存问题本次已从根上修掉（指纹化 URL），理论上刷新即可；若仍不对，再执行 `sudo synoservice --restart nginx` 清掉 nginx 的静态资源缓存。

> 说明：`1.0.11-0003` 及更早版本里，顶栏 logo 被 `max-age=86400` 缓存住，升级后最长要等 24 小时才会自己变过来 —— 这是本次修复的第 3 条。
