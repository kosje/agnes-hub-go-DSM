基于 [my788525/agnes-hub-go](https://github.com/my788525/agnes-hub-go) **v1.0.11** 构建的群晖 DSM 套件。

### 本次更新（1.0.14）

1. **修复生视频一直报 400 —— 上游的原话是 `mode is required`**（本次最重要的修复）：

   查了 Agnes 官方文档才明白，**视频模型分成两代，参数口径完全不兼容**：

   | | Agnes Video 2.5（当前推荐） | Agnes Video V2.0（上游 9/25 下线） |
   |---|---|---|
   | `mode` | **必填**：`text` / `keyframe` / `reference` | 可选：`ti2vid` / `keyframes` |
   | 时长 | `seconds` 字符串 `"4"`~`"12"` | `num_frames` + `frame_rate` |
   | 分辨率 | `size` 档位 `720P`/`1080P`/`1K`/`2K` | `width` + `height` 像素 |
   | 画幅 | `aspect_ratio`（默认 `16:9`） | — |
   | 关键帧 | `first_frame` / `last_frame` | `extra_body.image` |
   | 参考素材 | `images` / `audios` / `videos` | `image` |
   | 明确禁止 | **`width` / `height` / `num_frames` / `frame_rate` 一律 400** | — |

   而网关的字段白名单**只有一份，而且是「v2.0 味」的**，里面根本没有 `mode`。所以：

   - 选 `agnes-auto` → 自动挑中 `agnes-video-2.5-flash` → 缺 `mode` → `mode is required`；
   - 就算补上 `mode`，白名单里的 `width` / `num_frames` 一并发过去，**还会再吃一个 400**。

   现在按模型家族分两套口径，并且：
   - `mode` 会**按你提供的素材自动推断** —— 只给提示词就是 `text`，给了首/尾帧就是 `keyframe`，
     给了参考图就是 `reference`；你显式指定了且素材能满足，就尊重你的指定；
   - 与当前 `mode` 冲突的媒体字段会自动清掉（上游对「mode 与媒体字段不匹配」也是直接 400）；
   - `duration` 会自动映射成 2.5 要的 `seconds`；
   - 请求体改为**按最终选中的模型重建**，避免「换了模型名却没换参数」导致跨家族 400。

2. **顺带挖出并修掉一个更隐蔽的缺陷**：JSON 数组型字段被静默丢弃。

   代码里判断「这个字段是不是空的」用的是只认标量的转换函数，遇到 JSON 数组会返回空串 ——
   而请求体解析成 `map[string]any` 之后，`"images": ["https://..."]` 正好就是数组。
   结果是**参考图 / 多图输入这类最正常的写法会被当成空值悄悄丢掉**，用户完全看不出原因。
   这个缺陷生图链路同样受影响，一并修了。

3. 控制台账号页的「账号测试」按钮（探测）同步改用统一口径，否则测 2.5 视频同样会误报失败。

---

### 上一版（1.0.13）

1. **版本号规则改为纯 `x.y.z`**：发新版直接 +1，不再带 `-0001` 构建号。
   套件 INFO、套件源 catalog、控制台里显示的「当前版本 / 最新版本」、GitHub Release 的 tag
   全部来自同一个值，不会再出现「装的是 1.0.12-0002、界面显示 1.0.12」这种对不上的情况。

2. **生成的图片改为落盘保存，聊天窗口显示本地链接**：服务端把图片取回来存到
   `<数据目录>/images/`，正文里只放一个短短的本地地址。响应和聊天记录都小了一个数量级，
   而且翻聊天记录时图片还能重新显示出来。缓存目录有容量上限，超出自动淘汰最旧的。

3. **修复生视频链路的两个缺陷**：网页提交的视频以前不落任务记录、前端也拿不到 `job_id`；
   查询任务的路由会被当成「再提交一次」发给上游，视频即使提交成功也永远等不到结果。

4. **上游错误信息不再只剩一个「HTTP 400」**：兼容 `message` / `msg` / `detail` /
   `error` 是字符串等各种字段名，把上游原话挖出来显示。这次能查到 `mode is required`
   就是靠它。

5. **新增控制台「向上游拉取可用模型」**（账号池 → 账号列表右上角）：拿每个账号自己的 Key
   去问上游 `GET /v1/models`，把上游实际给的和你本地清单填的对一遍。
   这次也是靠它确认了「视频模型是可用的」，从而把排查方向从「模型下架」纠正到「参数口径」。

6. **旧品牌标识全面清理**：指标名 `agnes_hub_*`、cookie `agnes_hub_session`、
   会话头 `x-agnes-session`、User-Agent `agnes-hub/1.0` 等。
   升级后需要重新登录一次控制台与对话页（口令本身不受影响）。

---

### 推荐：添加群晖套件源（自动检查更新）

1. **套件中心** → **设置** → **常规** → **信任层级** 选择「**任何发行者**」；
2. **套件来源** → **新增**：
   - **x86_64 平台**（Intel / AMD 机型）：`https://kosje.github.io/agnes-hub-go-DSM/catalog.json`
   - **armv8 平台**（ARM64 机型，如 DS223/DS423 系列）：`https://kosje.github.io/agnes-hub-go-DSM/catalog-armv8.json`
3. 切换到套件中心「**社群**」标签页即可直接安装或一键更新。

---

### 手动安装下载

- **x86_64 机型**：[agnes-hub-x86_64-1.0.14.spk](https://kosje.github.io/agnes-hub-go-DSM/agnes-hub-x86_64-1.0.14.spk)
- **armv8 机型**：[agnes-hub-armv8-1.0.14.spk](https://kosje.github.io/agnes-hub-go-DSM/agnes-hub-armv8-1.0.14.spk)

---

### 升级后图标 / 页面还是旧的？

1. 在套件中心确认版本已是 `1.0.14`；
2. **强制刷新浏览器**（Windows `Ctrl+F5` / macOS `Cmd+Shift+R`），或换浏览器 / 无痕窗口验证；
3. 顶栏 logo 的缓存问题已从根上修掉（指纹化 URL），理论上刷新即可；若仍不对，再执行
   `sudo synoservice --restart nginx` 清掉 nginx 的静态资源缓存。
