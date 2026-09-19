# 媒体 / 音频接口对比

范围：图片生成/编辑、视频生成/编辑/延长、媒体输入/上传/文件、TTS、STT、voices、realtime/STT WebSocket、NSFW/spicy、错误映射、b64_json/url、base URL 拼接、存储媒体 TTL 与回收。

对照基线：codebase A = Orchids-2api（移植自 chenyme/grok2api 44a390b8）；codebase B = `.upstream/grok2api/backend`（HEAD 906b9493，v3.1.6）。A 是旧提交的移植，以下只列**实际分歧**，重构与命名差异不计。

## 结论摘要

共 45 条确认分歧（全部带双方逐行代码证据）：

- **P0（2 条）**：A5-1 `/images/edits` 线格式完全不同（A 仅 multipart，B 仅 JSON）；A5-22 chat `image_url` 服务端直连抓取无 SSRF 防护（B 有 netguard 公网校验 + 强制 https:443）。
- **P1（18 条）**：A5-6 客户端可自带 `nsfw` 直通上游；A5-9 媒体/音频端点只在 `/grok/v1` 而无 `/v1`（B 全在 `/v1`，直接破坏 drop-in 兼容）；A5-10 公开 base URL 取自客户端可伪造的 `X-Forwarded-*`/`Host`；A5-11 Web 免费档 6s 视频时长钳制缺失；A5-12 `/videos/generations` 硬绑 Console 账号池；A5-13 Console 资产下载外发 SSO Cookie；A5-14 创建失败不换号重试；A5-15 `/videos` 字段名不同且静默吞掉 `duration`/`aspect_ratio`；A5-23 附件数量/总量无上限；A5-24 视频上传票据先消费后校验（失败即烧毁）；A5-25 上传上限 512 MiB/400 对 256 MiB/413；A5-26 媒体读取端点路径/方法/鉴权不一致；A5-27 管理端缓存预览 URL 恒 404；A5-28 媒体输入无容量配额且过期文件永不回收；A5-29 媒体输入上传端点契约不同；A5-32 TTS/STT 上游 Content-Type 不校验直接透传；A5-33 上游 401/402/403 原样直达；A5-34 错误体 `type` 恒为 `invalid_request_error`。
- **P2（20 条）/ P3（5 条）**：图像响应字段与 URL 形态、图像默认值与 `resolution` 校验、图像模型目录、流式事件 `size`、视频 `user`/画幅/1080p 判定/前缀别名/错误码/相对 URL、Console 403 clearance、ftyp 误判、票据 TTL、错误体泄漏、voices 归一化、STT 状态码/采样率、无账号重试、大视频整段入内存、voice 上限与文案等。

最需要先修的是 A5-1（P0 线格式）、A5-22（P0 SSRF）、A5-9（P1 全局路径与自链接）、A5-13（P1 凭证外发）、A5-32/A5-33（P1 语音透传与凭据失败未屏蔽）、A5-24（P1 上传票据烧毁）。

## 发现

### A5-1 [P0] `/v1/images/edits` 线格式完全不同：A 仅 multipart，B 仅 JSON
- 本项目：`internal/grok/handler_image_edits.go:178` — `if err := r.ParseMultipartForm(80 << 20); err != nil {`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:585` — `if !isJSONRequest(c) {`（下一行即 `writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片编辑仅支持 application/json")`）
- 差异/错误：A 的编辑入口只解析 multipart/form-data，字段名走 `image` / `image[]`（handler_image_edits.go:258-261），`prompt`/`model`/`n`/`size`/`aspect_ratio`/`response_format` 全部取 `r.FormValue`（183-245）；B 的 `editImage` 先 `isJSONRequest`，非 `application/json` 一律 415，图像以 JSON `image`/`images` 数组的 `{url,file_id}` 传入（handler.go:154-168、609-630），并显式拒绝 `file_id`（handler.go:619-621）。B 全仓无任何 multipart 图片编辑路径（`ParseMultipartForm` 仅出现在 voice_handler.go:143 与 account/handler.go:1097）。
- 影响：同一客户端调用 `/v1/images/edits`：按 B 契约发 JSON 在 A 上得到 400「invalid multipart form」；按 A 契约发 multipart 在 B 上得到 415。两个实现无法互为替代，且 A 的 `image_url`+base64 编辑契约与 B 的 `image.url` 完全不可互译。
- 修复：A 增加 JSON 分支（`image`/`images` 数组，`{url,file_id}`），或至少同时接受 `application/json` 与 multipart；B 若需 multipart 兼容也应显式支持。

### A5-2 [P2] `/images/generations` 缺省 model：A 静默补 `grok-imagine-image`，B 返回 400
- 本项目：`internal/grok/types.go:707-710` — `func (r *ImagesGenerationsRequest) Normalize() {` / `if strings.TrimSpace(r.Model) == "" { r.Model = "grok-imagine-image" }`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:398-400` — `if decodeSingleJSON(bytes.NewReader(body), &request, false) != nil || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.Prompt) == "" { writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片请求缺少有效 model 或 prompt")`
- 差异/错误：A 在 `HandleImagesGenerations` 里先 `req.Normalize()`（handler_images.go:91），空 model 被替换成 `grok-imagine-image` 后继续生成；B 把空 model 与空 prompt 视作同一类校验失败直接 400。B 的 `n` 缺省为 1（handler.go:406），A 同样为 1（types.go:711-713），这部分一致。
- 影响：漏传 model 的请求在 A 上成功并消耗一次图片额度，在 B 上是明确错误；客户端无法用同一套参数校验逻辑。
- 修复：A 去掉 model 默认值（或仅在 `image_config` 桥接路径保留），缺 model 时返回 400。

### A5-3 [P2] 图像成功响应体字段集不同（`mime_type` / `revised_prompt` / `usage`）
- 本项目：`internal/grok/handler_image_helpers.go:245-248` — `data = append(data, map[string]interface{}{ field: val, "revised_prompt": nil, })`（外层 handler_image_helpers.go:250-254 另加 `"usage": buildImageUsagePayload(...)`）
- grok2api：`B:backend/internal/infra/provider/web/image.go:1486-1489` — `if format != "b64_json" { return map[string]any{"url": a.assets.PublicImageURL(asset.ID), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil }` / `return map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil`
- 差异/错误：B 每个 data 项固定含 `mime_type`，且 `revised_prompt` 是空串；B 的整体响应只有 `{"created","data"}`（image.go:1471）。A 不含 `mime_type`，`revised_prompt` 为 `null`，并且额外注入一个自行估算的 `usage` 对象（含 `input_tokens_details`/`completion_tokens_details` 等）。
- 影响：严格反序列化（`revised_prompt string`、必填 `mime_type`）的 OpenAI 客户端在 A 上 `null` → 解析失败或空指针；A 额外出现的 `usage` 在 B 上不存在，跨实现的响应比对/计费对账会不一致。
- 修复：A 补 `mime_type`（取自缓存时的 Content-Type 或 `http.DetectContentType`），`revised_prompt` 用 `""`；若保留 `usage` 应在文档中显式标注为 A 扩展。

### A5-4 [P2] 图片编辑 `resolution` 与像素别名 `aspect_ratio` 校验不一致
- 本项目：`internal/grok/handler_image_edits.go:220-228` — `if _, err := normalizeImageEditSize(r.FormValue("size")); err != nil {...}` / `if _, err := normalizeImageAspectRatio(r.FormValue("aspect_ratio"), r.FormValue("size")); err != nil {...}`（非 Console 分支完全不读 `resolution`）
- grok2api：`B:backend/internal/infra/provider/web/image.go:808-813` — `resolution := strings.ToLower(strings.TrimSpace(request.Resolution))` / `if resolution == "" { resolution = "1k" }` / `if resolution != "1k" { return invalidImageRequest("Grok Web 图片编辑当前仅支持 resolution=1k") }`
- 差异/错误：B 的传输层校验 `resolution ∈ {1k,2k}`（handler.go:661-668），Web provider 再把非 `1k` 拒绝为错误。A 的 Web 编辑路径只把 `resolution` 传给 Console 分支（handler_image_edits.go:316），非 Console 分支**静默忽略**，`resolution=2k` 的编辑请求照常成功。另外 A 的 `normalizeImageAspectRatio` 接受 `1280x720`/`1024x1024` 等像素别名（util_media.go:466-468），而 B 的 `validImageAspectRatio` 只接受纯比例串（handler.go:1067-1074）。
- 影响：`resolution=2k` 在 A 被吞掉后按 1k 出图且无任何提示，在 B 是明确 400；`aspect_ratio="1280x720"` 在 A 成功、在 B 400。客户端拿不到一致的能力边界。
- 修复：A 非 Console 编辑分支显式校验 `resolution`（空→1k，非 1k→400），并对齐 `aspect_ratio` 白名单。

### A5-5 [P2] 图像 URL 形态：A 可能回泄漏上游 URL 且为相对路径，B 恒为配置域名下的绝对资产 URL
- 本项目：`internal/grok/handler_image_helpers.go:235-243` — `if field == "url" && (!strict || !mustCacheImageURL(u)) { val = u }` / `if field == "url" && publicBase != "" && strings.HasPrefix(val, "/") { val = publicBase + val }`
- grok2api：`B:backend/internal/infra/provider/web/image.go:1486-1487` — `if format != "b64_json" { return map[string]any{"url": a.assets.PublicImageURL(asset.ID), ...`（service.go:249 `return s.runtimeConfig().PublicBaseURL + "/v1/media/images/" + id`）
- 差异/错误：B 的 url 格式**总是**先落本地资产、再返回配置 `PublicBaseURL` 拼出的绝对地址，从不回传上游地址。A 的图像编辑走 `strict=false`（handler_image_edits.go:162），缓存失败且 URL 不属于必须缓存的 Grok/X 主机时直接返回上游原始 URL；成功时返回相对路径 `/grok/v1/files/image/<sha1>.<ext>`（handler_image_helpers.go:200/206），只有 `publicBase != ""` 时才补全，而 `publicBase` 来自请求头（见 A5-10）。
- 影响：客户端可能拿到需要自行拼接的站内相对路径、或需要能直连 `assets.grok.com` 的原始 URL（A 自身的注释也承认部分网络环境不可达）；B 的客户端永远拿到可直接播放/展示的绝对地址。
- 修复：A 统一返回基于配置的绝对地址；对必须本地化的上游资产保持严格失败语义，不回退原始 URL。

### A5-6 [P1] 请求体 `nsfw` 由客户端控制并直通上游 `enable_nsfw`，B 仅由服务端配置控制
- 本项目：`internal/grok/types.go:128` — `NSFW           *bool           `json:"nsfw,omitempty"``（消费点 handler_images.go:225 `nsfw := req.NSFW != nil && *req.NSFW`，最终写入 imagine_ws.go:79 `"enable_nsfw": nsfw`）
- grok2api：`B:backend/internal/infra/provider/web/image.go:709` — `if err := connection.WriteJSON(imagineRequestMessage(newWebID("img"), request.Prompt, ratio, cfg.AllowNSFW, modelConfig.Pro, modelConfig.ExpectedCount)); err != nil {`
- 差异/错误：B 的 `imageGenerationRequest`（handler.go:135-147）没有 nsfw 字段，`enable_nsfw` 唯一来源是服务端配置 `cfg.AllowNSFW`（adapter.go:28，由 `cfg.Provider.Web.AllowNSFW` 注入，app/application.go:464）。A 在公开 `/images/generations` 上直接接受客户端 `"nsfw": true` 并传给 Grok Imagine WebSocket；A 自己的 `PublicImagineNSFW()` 配置（config.go:477-478，默认 true）只用于 admin/`public_api` 路径（admin_imagine.go:237、public_api.go:133）与删除 payload 中的 nsfw（admin_imagine.go:201-219），**不参与** `/images/generations` 的判定。
- 影响：任何持 key 的调用方都能单方面开启上游 NSFW 生成，绕过运营方的内容开关；B 的同一开关只掌握在管理员手中。
- 修复：A 用 `configSnapshot().PublicImagineNSFW()` 覆盖/合取请求里的 `nsfw`，或直接移除该请求字段。

### A5-7 [P2] 图像模型目录分歧：A 多出 web `-pro`/`-quality`，映射与 B 的 Console 同名产品冲突，且缺 `-quality-2.0` 别名
- 本项目：`internal/grok/models.go:77` — `{ID: "grok-imagine-image-quality", Name: "Grok Imagine Image Quality", UpstreamModel: "grok-imagine-image-quality-lite", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true}`（同文件 78 行另有 `grok-imagine-image-pro`；104 行 `grok-imagine-image-quality-2.0` 不存在）
- grok2api：`B:backend/internal/infra/provider/console/catalog.go:42` — `{PublicID: "grok-imagine-image-quality", UpstreamModel: "grok-imagine-image-quality", Capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}}`（别名见 console/catalog.go:59 `consoleAlias("grok-imagine-image-quality-2.0", "grok-imagine-image-quality", "grok-imagine-image-quality", "")`）
- 差异/错误：同一个公开模型名 `grok-imagine-image-quality`，B 走 Console 媒体 API（上游模型名就是 `grok-imagine-image-quality`），A 走 Web/App-Chat Imagine 路径且上游名被换成 `grok-imagine-image-quality-lite`。A 额外暴露 Web 版 `grok-imagine-image-pro`（B 的 web/console 目录均无此公开产品，见 web/catalog.go:18-32、console/catalog.go:33-51）；A 也不接受 B 的兼容别名 `grok-imagine-image-quality-2.0`（A 的接受列表见 handler_images.go:102，命中不了 ResolveModel 即 400）。
- 影响：按 B 目录调用 `grok-imagine-image-quality` 的客户端在 A 上落到完全不同的上游通道与计费口径；迁移到 A 后 `grok-imagine-image-quality-2.0` 直接 400。
- 修复：把 `grok-imagine-image-quality` 的公开路由对齐 B（Console + 上游 `grok-imagine-image-quality`），补 `-quality-2.0` 别名，或把 A 私有的 web 版改名以免与官方产品同名。

### A5-8 [P3] 流式图像事件 `size` 恒为 `auto`，B 回真实像素尺寸
- 本项目：`internal/grok/handler_images.go:237` — `"created_at": time.Now().Unix(), "size": "auto", "quality": "auto",`（completed 事件同款见 253-255 行）
- grok2api：`B:backend/internal/infra/provider/web/image.go:1614-1622` — `width, height := image.Width, image.Height` / `size := "auto"` / `if width > 0 && height > 0 { size = fmt.Sprintf("%dx%d", width, height) }` / `value := map[string]any{ "type": eventType, "b64_json": ..., "created_at": ..., "size": size, "quality": "auto", "background": "auto", "output_format": imageOutputFormat(raw), }`
- 差异/错误：B 在能解析出图像尺寸时给 `size` 填 `WxH`，A 的所有 partial/completed 事件都写死 `"auto"`。
- 影响：依赖 `size` 做布局/校验的客户端在 A 上永远拿不到实际尺寸。
- 修复：A 复用 `imageDimsFromBytes`（handler_image_helpers.go:83-92）回填真实尺寸。

### A5-9 [P1] 媒体/音频端点只在 `/grok/v1` 暴露，B 全在 `/v1`；且 A 返回的内容 URL 指向未注册路径
- 本项目：`cmd/server/routes.go:171-205` — `registerWithPrefixes(mux, grokPrefixes, "/images/generations", ...)` … `registerWithPrefixes(mux, grokPrefixes, "/audio/transcriptions", ...)`（`grokPrefixes := []string{"/grok/v1"}`，routes.go:126）以及 `internal/grok/handler_videos.go:88` — `"url": "/v1/videos/" + j.ID + "/content",`
- grok2api：`B:backend/internal/transport/http/server.go:176` — `v1 := router.Group("/v1")`（server.go:195 `inferenceHandler.Register(v1)`）与 `B:backend/internal/transport/http/inference/handler.go:97-98` — `router.GET("/videos/:requestId", h.getVideo)` / `router.GET("/videos/:requestId/content", h.getVideoContent)`
- 差异/错误：B 把 images/videos/files/tts/stt/audio/realtime 全部挂在 `/v1` 下（handler.go:92-107）。A 只把它们注册在 `/grok/v1`（`allPrefixes` 虽含 `/v1`，routes.go:81-83，但媒体路由用的是 `grokPrefixes`）；`/v1/<media>` 会先过 `v1Guard`（routes.go:440-442）再由 mux 的 `/` 兜底 `http.NotFound`（routes.go:361-364）。更糟的是 A 自己在标准视频响应里返回 `/v1/videos/{id}/content`（handler_videos.go:88），该路径从未注册，等于返回一个 404 的自链接；`toMap` 的 `/grok/v1/videos/{id}/content`（handler_videos.go:70）才是可达的，且两者都是相对路径（未使用 job.PublicBaseURL，handler_videos.go:435）。
- 影响：把 base_url 指向 `/v1` 的 OpenAI/官方 SDK 客户端在 A 上所有媒体与音频调用 404，即使只做视频轮询也会因为拿到不可达的 `url` 而失败；B 无此问题。
- 修复：为 `/v1` 注册同一批媒体/音频 handler（至少 images/videos/files/tts/stt/audio/realtime），并让返回 URL 使用 `PublicBaseURL` 拼绝对地址、指向真实注册的路径。

### A5-10 [P1] 返回媒体的绝对地址取自客户端可伪造的请求头（Host / X-Forwarded-*），B 取自服务端配置
- 本项目：`internal/grok/handler_chat.go:117-133` — `proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))` … `host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))` / `if host == "" { host = strings.TrimSpace(r.Host) }` / `return proto + "://" + host`（使用点 handler_images.go:149、handler_videos.go:435）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:936-944` — `func (h *Handler) publicURL(path string) string { baseURL := h.publicAPIBaseURL; if h.publicBaseURL != nil { baseURL = strings.TrimRight(strings.TrimSpace(h.publicBaseURL()), "/") } ... return baseURL + path }`
- 差异/错误：B 的公开地址只来自配置/运行设置（server.go:191 `deps.PublicAPIBaseURL`，settings/service.go:227-231），B 全仓不读取 `X-Forwarded-Host`/`X-Forwarded-Proto`。A 的 `detectPublicBaseURL` 优先信 `X-Forwarded-*`，其次 `Host`，无任何可信代理白名单校验（对比 B 有 `TrustedProxies` 配置但仅用于客户端 IP）。
- 影响：攻击者发一个带 `X-Forwarded-Host: evil.example` 的图像/视频请求，A 就会把响应里的 `url`/`content_url` 写成 `http(s)://evil.example/...`（Host 头注入），可用于污染缓存/前端渲染/诱导客户端把凭据发往攻击者域；B 无此面。
- 修复：改用配置的 public base URL；若必须支持反代，只在 `TrustedProxies` 命中的对端才采信 `X-Forwarded-*`。

### A5-11 [P1] Web/免费档视频时长 6 秒钳制缺失，A 直接把超限时长发给上游
- 本项目：`internal/grok/util_media.go:385` — `if cfg.VideoLength < 1 || cfg.VideoLength > 15 {`（随后 handler_chat.go:512 `"duration": videoCfg.VideoLength` 原样构造上游 payload）
- grok2api：`B:backend/internal/infra/provider/web/video.go:270` — `seconds := applyFreeWebVideoDurationCap(request.Duration, cfg.FreeVideoDurationCap, request.Credential)`（钳制实现 video.go:525-540；默认值 `DefaultWebFreeVideoDurationCap = 6`，settings.go:19）
- 差异/错误：B 对 `WebTierBasic` 凭证在发请求前把时长压到 6 秒（可配 1..15），Super/Heavy 不钳制。A 全仓没有 `WebTier`/`FreeVideoDurationCap` 概念（`grep -rn "FreeVideoDurationCap\|WebTier" internal/ cmd/` 无命中），只做 1..15 的通用校验。
- 影响：基础/免费 Web 账号请求 >6 秒时，A 把超限时长送到上游换来 429，异步任务直接 failed；B 在本地钳制，避免无意义的失败与账号轮换。
- 修复：移植 `FreeVideoDurationCap`（默认 6、可配 1..15），在 Web/AppChat 生成路径按账号档位钳制后再构造 payload。

### A5-12 [P1] `/videos/generations` 在 A 硬绑 Console 账号池，Web 视频账号部署必然 503
- 本项目：`internal/grok/handler_videos_console.go:481` — `return isGrokConsoleAccount(account) && AccountSupportsModel(account, model) && h.routeAllowsAccount(ctx, model, account.ID)`（取不到账号即 `internal/grok/handler_videos_console.go:232` `sess, err := h.openConsoleVideoAccountSession(...)` → 503 account_unavailable）
- grok2api：`B:backend/internal/application/gateway/video.go:137` — `routes, _, err = s.eligibleMediaRoutes(routes, input.ClientKey, model.CapabilityVideo, providerSupported)`（providerSupported 由 `s.providers.Videos(providerValue)` 覆盖 Build/Web/Console，video.go:133-136）
- 差异/错误：A 的官方视频端点（routes.go:174）只从 Console 池取号，不读模型存储路由的 provider；而 A 给 `grok-imagine-video` 的默认路由是 **web**（store.go:713-720，未命中 `console/`/`build/` 前缀即 `model.Provider = "web"`），Web 生成只能走 A 私有的 `POST /grok/v1/videos`（routes.go:173）。B 在同一端点上按能力收集全部 provider 路由——web 目录本身就把 `grok-imagine-video` 注册为视频路由（web/catalog.go:31）。
- 影响：只配了 Web 视频账号的部署里，B 可直接 `POST /v1/videos/generations`，A 必然 503，客户端必须改调非标准端点，drop-in 兼容失败。
- 修复：该端点按模型路由/能力选择 provider（Web 路由复用 `HandleVideosCreate` 的实现），或把 `/videos` 的 Web 能力并入官方端点。

### A5-13 [P1] Console 成品视频下载在 A 外发 SSO Cookie / 身份头到资产域，B 为匿名 GET
- 本项目：`internal/grok/handler_videos_console.go:853` — `request.Header = c.assetDownloadHeaders(token, rawURL)`（判定见 client.go:350 `if c.shouldSendAuthForAssetURL(link) {`，而 client.go:333 `if host == "grok.com" || strings.HasSuffix(host, ".grok.com") || host == "x.ai" || strings.HasSuffix(host, ".x.ai") {`）
- grok2api：`B:backend/internal/infra/provider/console/media.go:695` — `request.Header.Set("Accept", "video/*,*/*;q=0.8")`（B 的 Build 侧注释同旨：cli/video.go:246 `// 资源域不需要 OAuth；不得解密或转发 token 与客户端身份头。`、cli/video.go:257 `// 仅保留 egress 路径上的匿名 GET；禁止 Authorization / Token-Auth / 会话身份头。`）
- 差异/错误：A 把任何 `*.x.ai`（含 `vidgen.x.ai`）视为需要鉴权的资产域，于是对视频内容 URL 附加 `Cookie: sso=…; sso-rw=…; cf_clearance=…` 等身份头；B 的 Console/Build 资产下载只设 Accept/User-Agent，绝不携带 token 或会话身份头。
- 影响：会话凭证被复制到媒体 CDN 域，扩大了凭证暴露面（CDN/中间层日志即可收集 SSO Cookie）；与 B 刻意维持的"资产域匿名 GET"安全策略直接相反。
- 修复：Console/Build 资产下载改为无凭证 GET（仅 Accept/UA），或把 `vidgen.x.ai` 从需鉴权资产域名单移除。

### A5-14 [P1] 视频创建阶段失败在 A 直接判死任务，B 分类重试/换账号
- 本项目：`internal/grok/handler_videos_console.go:546` — `h.failVideoJob(job, err)`（Web 路径同 handler_videos.go:484-487）
- grok2api：`B:backend/internal/application/gateway/video.go:544` — `for attempt := 0; attemptPolicy.allows(attempt); attempt++ {`（create 阶段错误分类 video.go:641-694，poll/后处理失败才终止 video.go:700）
- 差异/错误：A 的 Console/Build 创建与轮询错误统一走 `handleConsoleVideoJobError → failVideoJob`，首个错误即置 `failed`；B 对 401/403/402/429 等创建期错误判定为可重试并换账号，只有轮询/后处理失败才终止。
- 影响：一次瞬时 429/403 就把异步任务判死，客户端只能整单重投，成功率与配额利用率低于 B。
- 修复：A 的创建阶段引入 attempt policy，按 provider 错误分类决定是否释放租约并换账号重试。

### A5-15 [P1] A 的 `/videos` 用另一套字段名并静默丢弃 `duration`/`aspect_ratio`/`resolution`，默认值与 B 完全不同
- 本项目：`internal/grok/handler_videos.go:289` — `if err := json.NewDecoder(r.Body).Decode(&req); err != nil {`（`VideosRequest` 只认 `seconds`/`size`/`resolution_name`/`preset`，types.go:76-84；默认 `Size: firstNonEmpty(req.Size, "720x1280")`，handler_videos.go:401）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:729` — `if err := decodeSingleJSON(c.Request.Body, &request, true); err != nil {`（`duration` 缺省 8：handler.go:1024 `value := 8`；`aspect_ratio` 缺省 16:9：handler.go:780-782）
- 差异/错误：B 只有一个契约（`duration`/`aspect_ratio`/`resolution`/`image`/`reference_images`/`reference_audios`/`video`），且 `DisallowUnknownFields`，未知字段直接 400。A 的 `/videos` JSON 分支不做 DisallowUnknownFields，也没有 `duration`/`aspect_ratio`/`resolution` 字段——这些键被静默丢弃，落到默认 6 秒（types.go:739-741）与 720x1280 → 9:16；只有 form/multipart 分支才映射 `video_length`/`aspect_ratio`（handler_videos.go:300-301）。此外 A 校验 `preset ∈ {fun,normal,spicy,custom}`（util_media.go:397-402），但 create 路径并不使用它（handler_chat.go:577-582 固定 `--mode=custom`），B 没有 preset 字段。
- 影响：按 B schema 提交 `{"duration":10,"aspect_ratio":"16:9"}` 的客户端在 A 上得到 200 与 6 秒 / 9:16 的成品，参数被静默吞掉且无任何提示；preset 被接受却无效。
- 修复：`/videos` 接受 `duration`/`aspect_ratio`/`resolution` 别名，未知字段返回 400；或废弃该路由统一到 `/videos/generations`。

### A5-16 [P2] 视频失败响应错误码恒为 `internal_error`，B 会映射账号/模型类错误
- 本项目：`internal/grok/handler_videos.go:105` — `"status": "failed", "error": map[string]interface{}{"code": "internal_error", "message": message},`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1107` — `"error":  gin.H{"code": officialVideoErrorCode(job.ErrorCode), "message": job.ErrorMessage},`（映射表 handler.go:1114-1123：`account_unavailable`/`provider_unavailable`→`service_unavailable`，`model_not_found`→`invalid_argument`）
- 差异/错误：A 丢弃任务级错误码，把账号不可用等一律渲染成 `internal_error`；B 保留并映射。
- 影响：客户端无法区分"上游/账号暂不可用（可重试）"与"内部错误"，重试与告警策略误判。
- 修复：把 A 的任务错误码按同类规则映射后再输出。

### A5-17 [P2] A 的官方视频端点拒绝 `user` 字段
- 本项目：`internal/grok/handler_videos_console.go:196` — `decoder.DisallowUnknownFields()`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:182` — `User            *string                `json:"user"``
- 差异/错误：A 的 `consoleVideoAPIRequest` 与 B 的 `videoGenerationRequest` 其余字段一一对应，唯独没有 `user`，而 A 开了 `DisallowUnknownFields`，于是带 `"user"` 的请求 400。
- 影响：按 B 官方 schema（含 `user`）迁移的客户端在 A 被 400 拒绝。
- 修复：补 `User *string `json:"user"``（忽略或透传）。

### A5-18 [P2] Web 视频不支持 4:3 / 3:4 画幅
- 本项目：`internal/grok/util.go:120-131` — `videoAspectRatioMap = map[string]string{ "1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1024x1792": "2:3", "1024x1024": "1:1", ... }`（无 4:3/3:4；错误文案见 util_media.go:381）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:1058-1062` — `func validVideoAspectRatio(value string) bool { switch value { case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3": return true`
- 差异/错误：A 的 Web `/videos` 对 4:3/3:4 一律 400；B 在 HTTP 层接受并透传。A 自己的 Console 端点却支持 4:3/3:4（handler_videos_console.go:300），两个入口能力不一致。
- 影响：4:3/3:4 的 Web 视频在 B 可用、在 A 不可用。
- 修复：把 4:3/3:4 加入 `videoAspectRatioMap`。

### A5-19 [P2] 1080p 资格判定用公开模型名，`build/grok-imagine-video-1.5` 被误拒
- 本项目：`internal/grok/handler_videos_console.go:309` — `if resolution == "1080p" && model != "grok-imagine-video-1.5" {`（`model` 来自 handler_videos_console.go:273 `model := normalizeModelID(request.Model)`）
- grok2api：`B:backend/internal/application/gateway/video.go:261` — `if trimmedModel != "grok-imagine-video-1.5" {`（`trimmedModel` 取路由的 UpstreamModel，video.go:240）
- 差异/错误：A 比较的是公开 ID，因此 `build/grok-imagine-video-1.5`（models.go:82，上游模型正是 `grok-imagine-video-1.5`）被判"不支持 1080p"；B 用上游模型名判定，Build/Console 前缀的 1.5 都放行。A 的 `/videos` 入口用的是 `spec.UpstreamModel`（util_media.go:419-421），同一模型在两个入口结果不同。
- 影响：同一上游模型因公开 ID 前缀不同被拒 1080p；A 内两个入口行为不一致。
- 修复：ResolveModel 之后用 `spec.UpstreamModel` 做 1080p 判定。

### A5-20 [P2] 带 Provider 前缀的视频模型别名不被接受
- 本项目：`internal/grok/models.go:113` — `id = strings.TrimPrefix(id, "web/")`（114 行随即精确查表；表中无 `console/grok-imagine-video` 等项）
- grok2api：`B:backend/internal/domain/model/model.go:171` — `return [][]string{{literal}, {qualified}}`（前缀比较用 `strings.EqualFold`，model.go:162）
- 差异/错误：A 只把 `web/` 当别名剥离，`console/grok-imagine-video`、`Console/grok-imagine-video-1.5` 直接 400"not a video model"；B 先按字面外部名匹配，再回退 Provider 命名空间写法且大小写不敏感。
- 影响：从 B 迁移、使用 `Console/…` 前缀调用视频的客户端在 A 报错。
- 修复：剥离任意 provider 前缀后再查表，或补齐 console/ 前缀别名。

### A5-21 [P2] Console 403 挑战不失效/重建 clearance，A 持续复用坏出口
- 本项目：`internal/grok/dpop.go:354` — `recordUpstreamChallenge("cloudflare")`（该路径不失效 clearance 也不重试；A 的失效逻辑只在通用 doRequest 的 egress 分支，client.go:718-730）
- grok2api：`B:backend/internal/infra/provider/console/media.go:670` — `lease.InvalidateClearance()`（触发条件 media.go:667-671 `shouldInvalidateConsoleClearance(data)`，判定见 console/adapter.go:263-265）
- 差异/错误：A 的 `doConsoleDPoPRequestWithHeaders` 命中 Cloudflare 403 只累加计数，不使 clearance 失效、不重试；B 会失效对应 lease 的 clearance 并由上层重试重建。
- 影响：一旦命中 CF 403，A 会继续复用同一条坏 clearance/出口，后续视频与图片请求持续失败。
- 修复：在确认上游挑战为 cloudflare 时失效对应 clearance 并重试一次。

### A5-22 [P0] chat `image_url` 在服务端直连抓取，无 SSRF 防护且允许明文 http
- 本项目：`internal/grok/util.go:460` — `req, err := http.NewRequest(http.MethodGet, u, nil)`（464 行 `resp, err := client.Do(req)`，473 行 `io.ReadAll(io.LimitReader(resp.Body, 60*1024*1024))`；唯一前置校验是 util_messages.go:298 `if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")` 与 handler_chat.go:612 `if isRemoteURL(data) {`）
- grok2api：`B:backend/internal/infra/provider/web/attachments.go:349` — `if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {`（369-373 行逐 IP 调用 `publicRemoteImageAddress` 拒绝私网/环回/链路本地）
- 差异/错误：A 用默认 transport 直连客户端给出的任意 `http(s)` URL，不解析、不校验目标 IP，不限制端口，也不禁 userinfo；B 强制 https:443、禁 userinfo，并对 DNS 解析出的每个地址做公网校验，且把连接固定到该 IP（`newRemoteImageTarget` 用 `net.JoinHostPort(address, "443")` 重写 Host）。
- 影响：持 key 的调用方在消息 `image_url` 填 `http://169.254.169.254/latest/meta-data/…` 或 `http://127.0.0.1:PORT/…` 即可让代理替其发起内网/元数据请求（SSRF、内网探测、云元数据读取）；B 已封堵。
- 修复：复用 netguard 逻辑（`IsPublicAddress` 校验、强制 https+443、禁 userinfo），并在 `DialContext` 二次校验。

### A5-23 [P1] 对话附件数量与总字节无上限（单条 60 MiB）
- 本项目：`internal/grok/handler_chat.go:586-591` — `func (h *Handler) uploadAttachmentInputs(...)` / `out := make([]string, 0, len(inputs))` / `for _, item := range inputs {`（单条上限 util.go:473 `io.ReadAll(io.LimitReader(resp.Body, 60*1024*1024))`）
- grok2api：`B:backend/internal/infra/provider/web/attachments.go:61` — `if len(inputs) > maxChatAttachments {`（常量 `maxChatAttachments = 8`，attachments.go:23；84 行 `if size > maxChatAttachmentTotal || total > maxChatAttachmentTotal-size {`，`maxChatAttachmentTotal = 64 << 20`，attachments.go:24）
- 差异/错误：B 单请求最多 8 个附件、合计 64 MiB，且先看 Content-Length 再累计。A 对附件个数不设上限，只有每个 URL 各自的 60 MiB 上限，既无 Content-Length 预检也无合计预算。
- 影响：一次请求即可串行下载 N×60 MiB 进内存并 base64 放大约 1.33 倍，构成内存/带宽放大 DoS。
- 修复：加 8 个/合计 64 MiB/单条上限，先读 Content-Length 再做总量预算。

### A5-24 [P1] 视频上传票据在读取/校验前被消费，失败即烧毁且不可重试
- 本项目：`internal/grok/handler_video_upload.go:108` — `job, ok := h.consumeBuildVideoUpload(r.Context(), token)`（内部 handler_video_upload.go:75 `raw, err := h.lb.Store.RedisClient().GetDel(ctx, redisKey).Bytes()` 即删除；MIME 校验在其后的 113-119 行，读 body 在 121-125 行）
- grok2api：`B:backend/internal/application/media/upload.go:186` — `ticket, consumed, err := s.tickets.ConsumeUploadTicket(ctx, tokenHash, now)`（MIME 预检在其前 upload.go:174-177；消费后失败用 upload.go:197 `_, _ = s.tickets.ReleaseUploadTicket(context.WithoutCancel(ctx), tokenHash)` 归还）
- 差异/错误：A 先 GetDel/删除票据，再校验 Content-Type 与读取 body，因此 415/400/500 的失败请求都会永久废掉该上传地址；B 的类型预检在消费之前，且消费后失败会归还票据。
- 影响：任何一次失败/超限上传都会让该视频任务的上传回执地址永久 404，任务卡死无法完成；B 可重试。
- 修复：先做类型与长度校验再消费票据；消费后落盘或校验失败时归还票据。

### A5-25 [P1] 视频上传上限 512 MiB/400 对 B 的 256 MiB/413
- 本项目：`internal/grok/handler_videos_console.go:28` — `maxConsoleVideoAssetBytes    = 512 << 20`（超限路径 handler_video_upload.go:121-123 `raw, err := io.ReadAll(io.LimitReader(r.Body, maxConsoleVideoAssetBytes+1))` / `http.Error(w, "invalid video upload", http.StatusBadRequest)`）
- grok2api：`B:backend/internal/transport/http/middleware/request.go:77` — `const mediaUploadMaxBytes = 256 << 20`（超限映射 `B:backend/internal/transport/http/media/handler.go:124` — `c.Status(http.StatusRequestEntityTooLarge)`）
- 差异/错误：B 在中间件对该 PUT 路径套 256 MiB MaxBytesReader 并回 413；A 用 LimitReader 读到 513 MiB 才判定并回 400。
- 影响：同一超限上传状态码不同（400 vs 413），A 允许落盘体积是 B 的两倍，且超限时把整个 body 读完。
- 修复：对齐 256 MiB、改用 MaxBytesReader、超限返回 413。

### A5-26 [P1] 媒体读取端点路径/方法/鉴权不一致（B：`/v1/media/{images|videos}/:id` GET+HEAD+ETag；A：`/grok/v1/files/{kind}/{name}` 仅 GET 且需 key）
- 本项目：`cmd/server/routes.go:184` — `registerWithPrefixes(mux, grokPrefixes, "/files/", inferenceAuth(grokHandler.HandleFiles))`（`internal/grok/handler_files.go:53` — `if !requireMethod(w, r, http.MethodGet) {`）
- grok2api：`B:backend/internal/transport/http/media/handler.go:26-32` — `func (h *Handler) RegisterPublic(router *gin.Engine) {` / `router.GET("/v1/media/images/:assetId", h.getImage)` / `router.HEAD("/v1/media/images/:assetId", h.getImage)` / `router.GET("/v1/media/videos/:assetId", h.getVideo)` / `router.HEAD("/v1/media/videos/:assetId", h.getVideo)`（ETag/304 与 nosniff 见 handler.go:67-77）
- 差异/错误：B 是公开、以不可猜测资产 ID 寻址、GET+HEAD、带 ETag/If-None-Match→304 与 `X-Content-Type-Options: nosniff`；A 是以 sha1 文件名寻址、只允许 GET（HEAD 被 `requireMethod` 拒为 405）、无 ETag/304，且整条路由被 `inferenceAuth` 门禁（需客户端 API key）。
- 影响：按 B 契约实现的客户端（HEAD 探测、按 id 取图、ETag 缓存、匿名展示集成）在 A 上得到 404/405/401；两套媒体读取契约不可互换。
- 修复：补注册 `/v1/media/{images,videos}/:id`（GET+HEAD）、补 ETag/304/nosniff，并明确公开读取策略。

### A5-27 [P1] 管理端缓存列表返回的媒体 URL 指向从未注册的 `/v1/files/...`
- 本项目：`internal/grok/admin_cache.go:373` — `viewURL := "/v1/files/" + typ + "/" + name`
- grok2api：`B:backend/internal/application/media/service.go:249` — `return s.runtimeConfig().PublicBaseURL + "/v1/media/images/" + id`
- 差异/错误：A 只注册了 `/grok/v1/files/`（routes.go:184），`/v1/files/...` 会先过 v1Guard（routes.go:440-442）再落到 mux 的 `/` 兜底 `http.NotFound`（routes.go:361-364），因此 `ViewURL`/`PreviewURL`（admin_cache.go:373-377）恒为坏链（鉴权开启时先 401）；同文件 382 行用的却是 `/grok/v1/files/`，A 内部也不自洽。B 返回配置域名下的绝对地址且路由真实存在。
- 影响：管理端缓存列表每条预览/查看链接都不可用，且是相对路径无法跨域使用。
- 修复：改用 `/grok/v1/files/...`（或正式注册 `/v1/files/`），并用 `public_base_url` 拼绝对地址。

### A5-28 [P1] 临时媒体输入无容量配额，且 TTL 过期后磁盘文件永不回收
- 本项目：`internal/grok/handler_media_inputs.go:100` — `if err := h.lb.Store.SaveStoredMediaInput(r.Context(), input, mediaInputTTL); err != nil {`（TTL `mediaInputTTL = 24 * time.Hour`，handler_media_inputs.go:21；文件删除仅见 101 行回滚与 139 行显式 DELETE）
- grok2api：`B:backend/internal/application/media/service.go:129` — `if int64(len(data)) > capacityLimit || total > capacityLimit-int64(len(data)) {`（`B:backend/internal/application/media/service.go:521` — `expired, listErr := s.assets.ListExpiredMediaAssets(ctx, cleanupNow, expiredOffset, cleanupInputBatchSize)`）
- 差异/错误：B 写入前按 `cleanupThresholdBytes`/`MaxTotalBytes` 判容量（超限 `ErrMediaCapacity` → 507），并有后台 `Cleanup` 按 TTL 列表同时删除对象与元数据。A 只把记录写进 Redis 并设 24h TTL，落盘文件除显式 DELETE 与保存失败回滚外无任何删除路径（全仓 `os.Remove` 仅 handler_media_inputs.go:101/139、handler_image_helpers.go:315/319、admin_cache.go:631、media_storage.go:94），既无写入前总量检查，也无按过期时间的清扫。
- 影响：任何持 key 的调用方都能反复上传 20 MiB 输入写满磁盘；24h 后 Redis 记录过期而文件永久残留，磁盘只增不减。
- 修复：写入前做容量阈值检查并返回 507；增加按 `ExpiresAt` 清扫文件与元数据的后台任务。

### A5-29 [P1] 媒体输入上传端点/鉴权/JSON 契约完全不同
- 本项目：`cmd/server/routes.go:185` — `registerWithPrefixes(mux, grokPrefixes, "/media/inputs", inferenceAuth(limiter.Limit(grokHandler.HandleMediaInputs)))`（响应体 `internal/grok/handler_media_inputs.go:106` — `"file_id": id, "object": "file", "kind": kind, "mime_type": mimeType,`）
- grok2api：`B:backend/internal/transport/http/server.go:146` — `adminRoot := router.Group("/api/admin/v1")`（上传注册于受 AdminAuth 保护的 media/handler.go:42 `router.POST("/media/inputs/upload", h.uploadInputAsset)`；响应体 `B:backend/internal/transport/http/media/ingest.go:385` — `"fileId": asset.ID, "kind": asset.Kind, "mimeType": asset.MIMEType, "sizeBytes": asset.SizeBytes, "expiresAt": expiresAt,`）
- 差异/错误：A 的入口在推理前缀上（客户端 API key + 并发限流），返回扁平 snake_case（`file_id`/`mime_type`/`bytes`）；B 的入口在管理端（管理员会话），返回 `{"data":{...}}` 包裹的 camelCase（`fileId`/`mimeType`/`sizeBytes`/`expiresAt`）。错误体也不同：A `{"error":{"message","type","code"}}`（handler_responses_store.go:460-469），B `{"error":{"code","message","requestId"}}`（shared/response/response.go:27）。A 另有 `/media/inputs/{id}` 的 GET/DELETE，B 无公开对应端点。
- 影响：按 B 契约编写的管理端/工具在 A 上 404 或字段解析失败；错误码字符串（如 `media_too_large` 对 `mediaTooLarge`）不可互换。
- 修复：在 `/api/admin/v1/media/inputs/upload` 提供等价端点并对齐字段名与 `data` 包裹（或双写兼容）。

### A5-30 [P2] 任意 `ftyp` 载荷（HEIC/AVIF 等）被判为 `video/mp4`
- 本项目：`internal/grok/handler_media_inputs.go:182` — `if len(data) >= 12 && string(data[4:8]) == "ftyp" {`（187 行 `return "video", "video/mp4", nil`）
- grok2api：`B:backend/internal/transport/http/media/ingest.go:331` — `detectedMIME := strings.ToLower(http.DetectContentType(data))`（image 分支 ingest.go:332-334；仅 `strings.HasPrefix(declaredMIME, "video/") || strings.HasPrefix(detectedMIME, "video/")` 才走视频，ingest.go:335）
- 差异/错误：A 只看偏移 4..8 的 `ftyp` 魔数，不看 brand、也不要求声明类型是 `video/*`（仅 `video/quicktime` 例外），于是 HEIC/AVIF 这类同样以 `ftyp` 开头的容器被存成 `kind=video`、`mime=video/mp4` 并以 `data:video/mp4` 发往上游；B 要求嗅探或声明为视频，HEIC/AVIF（`DetectContentType` 返回 `application/octet-stream`，声明为 `image/*`）会被拒。
- 影响：图片文件被当作视频输入，非视频字节被送往上游；同一文件在 B 被拒、在 A 被接受。
- 修复：校验 ftyp brand（`isom`/`mp4 ` 等）或直接采用 `DetectContentType` 的 `video/mp4` 结果，并统一要求声明类型与嗅探一致。

### A5-31 [P2] 视频上传票据 TTL 1 小时（B 为 2 小时）
- 本项目：`internal/grok/handler_videos.go:25` — `const videoJobTTL = time.Hour`（票据以其为 TTL：handler_video_upload.go:53 `Set(context.Background(), redisKey, raw, videoJobTTL)`）
- grok2api：`B:backend/internal/application/media/upload.go:26` — `videoUploadTicketTTL = 2 * time.Hour`
- 差异/错误：A 复用视频任务 TTL 作为票据 TTL；B 为票据单设 2h 并在接收时显式判过期（upload.go:156）。
- 影响：上游回调在 1 小时后到达时 A 一律 404，B 仍有 1 小时窗口。
- 修复：为票据引入独立的 2h 常量。

### A5-32 [P1] TTS/STT 上游响应头不校验直接透传（含 Content-Disposition，且缺 nosniff/CSP，丢 Retry-After）
- 本项目：`internal/grok/handler_voice.go:745-751` — `func copyVoiceResponseHeaders(destination, source http.Header) {` / `for _, key := range []string{"Content-Type", "Content-Disposition", "X-Request-Id"} {` / `destination.Add(key, value)`（随后 handler_voice.go:703-711 `copyVoiceResponseHeaders(w.Header(), resp.Header)` 与 `io.Copy(w, resp.Body)`，无任何类型校验）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:498-519` — `func normalizeMediaResponseContentType(value string) (string, bool) {` … `case "audio/aac", "audio/flac", "audio/l16", "audio/mpeg", "audio/mp3", "audio/ogg", "audio/opus", "audio/pcm", "audio/wav", "audio/webm", "audio/x-flac", "audio/x-wav":`（非白名单/解析失败一律 `return "", false` → handler.go:468-472 回 502 `invalid_media_type`；安全头见 handler.go:521-529）
- 差异/错误：B 把上游类型限制在 `application/json`、`text/plain`、`application/ogg` 与 `audio/*` 白名单，并固定加 `X-Content-Type-Options`/CSP/Cache-Control，只回传 Retry-After 与 X-Request-Id。A 把上游 Content-Type 与 Content-Disposition（含上游文件名）原样写给客户端并直接拷贝流。
- 影响：Console 返回 `text/html`、`application/octet-stream` 或空类型时，A 会把未校验内容类型（及上游下载文件名）透给浏览器客户端且缺 nosniff/CSP，存在内容嗅探与文件名注入面；上游 429 的 Retry-After 在 A 上丢失。
- 修复：响应前做同一白名单校验（否则 502 `invalid_media_type`），补 nosniff/CSP，停止透传 Content-Disposition，透传 Retry-After。

### A5-33 [P1] 上游 401/402/403 未屏蔽，原状态码直达客户端
- 本项目：`internal/grok/handler_voice.go:737-740` — `if typed, ok := err.(*consoleVoiceRequestError); ok {` / `writeResponsesAPIError(w, typed.status, typed.code, typed.Error())`（`typed.status` 来自 handler_voice.go:732 `upstreamHTTPResponseStatus(err)`）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:457-461` — `if isUpstreamCredentialStatus(result.StatusCode) {` / `errorCode = "upstream_unavailable"` / `writeOpenAIError(c, http.StatusServiceUnavailable, clientCode, credentialErrorMessage(clientCode))`（判定 `isUpstreamCredentialStatus` 覆盖 401/402/403，handler.go:2402-2405）
- 差异/错误：A 把上游状态码当客户端状态码返回，Console 401/403 时客户端收到 401/403；B 统一改写为 503 `upstream_unavailable` + 固定文案。
- 影响：客户端把"账号/凭据失效"误判为自身鉴权失败并可能重试或泄露上游语义；B 明确屏蔽凭据类失败。
- 修复：voice 错误路径对 401/402/403 统一返回 503 `upstream_unavailable` 与通用文案。

### A5-34 [P1] 错误体 `type` 恒为 `invalid_request_error`，且缺 `param` 字段
- 本项目：`internal/grok/handler_responses_store.go:460-469` — `func writeResponsesAPIError(...)` … `"type":    "invalid_request_error",`
- grok2api：`B:backend/internal/transport/http/inference/handler.go:2275-2285` — `errorType := "invalid_request_error"` / `switch { case status == http.StatusUnauthorized: errorType = "authentication_error"; case status == http.StatusTooManyRequests: errorType = "rate_limit_error"; case status >= 500: errorType = "server_error" }` / `c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": errorType, "code": code, "param": nil}})`
- 差异/错误：A 所有 voice 错误的 `type` 都写死 `invalid_request_error` 且无 `param` 键；B 按状态映射 type 并附带 `param: null`。
- 影响：按 `type` 决定重试/换 key 的 OpenAI 客户端（把 429/5xx 当参数错误）在 A 上全部误判；缺 `param` 使错误对象反序列化不完整。
- 修复：对齐 B 的 type 映射并补 `param`。

### A5-35 [P2] 上游错误响应体被写进客户端可见的 `error.message`
- 本项目：`internal/grok/upstream_error.go:37` — `b.WriteString(" body=" + e.body)`（有界 `maxUpstreamBodyBytes=4096`，upstream_error.go:10；voice 链经 handler_voice.go:739 原样写出）
- grok2api：`B:backend/internal/application/gateway/voice.go:533` — `message = "上游语音服务返回错误"`（仅当错误实现 `PublicMessageError` 且文案非空才公开，否则中文兜底；并回 Retry-After，voice.go:536-539）
- 差异/错误：A 把上游状态码与响应体拼进 `error.message` 返回给调用方；B 只回分类化文案。
- 影响：Console 错误体（可能含账号/配额/内部提示）泄漏给任意持 key 的调用方；A 同时不回 Retry-After。
- 修复：voice 错误只回分类化文案，上游原文仅进日志/审计，并透传 Retry-After。

### A5-36 [P2] `/tts/voices` 原样透传，未归一化列表形状
- 本项目：`internal/grok/handler_voice.go:339` — `h.forwardConsoleVoice(w, r, modelID, http.MethodGet, path, nil, http.Header{"Accept": []string{"application/json"}})`
- grok2api：`B:backend/internal/application/gateway/voice.go:148-158` — `items := make([]map[string]any, 0, len(voices))` / `item := map[string]any{"voice_id": voice.VoiceID, "name": voice.Name}` / `if voice.Language != "" { item["language"] = voice.Language } else { item["language"] = nil }`（解析见 console/voice.go:132-141，单条同样重建，voice.go:177-182）
- 差异/错误：B 由 adapter 解析后重建每项 `{voice_id,name,language}`，`language` 缺失时显式给 `null`，未知字段丢弃；A 把 Console 响应字节原样转发，列表契约完全取决于上游。
- 影响：上游省略 `language` 时 A 少一个键而 B 返回 `"language": null`，严格类型/OpenAPI 生成的客户端在 A 上解析失败。
- 修复：A 侧解析并归一化 voices 列表与单条对象。

### A5-37 [P2] GET `/stt` 非 Upgrade：A 400 对 B 405
- 本项目：`internal/grok/handler_voice_ws.go:42` — `writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", "a WebSocket Upgrade request is required")`（`/stt` 与 `/realtime` 共用同一 handler，handler_voice.go:344-348）
- grok2api：`B:backend/internal/transport/http/inference/voice_ws_handler.go:34` — `writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request", "STT 流式接口需要 WebSocket Upgrade")`（B 的 `/v1/realtime` 仍为 400，voice_ws_handler.go:41-43）
- 差异/错误：A 对 `/stt` 与 `/realtime` 的非 Upgrade 请求一律 400；B 对 `/stt` 单列 405。
- 影响：用普通 GET 探测 `/stt` 的网关在 A 得 400（易判为参数错误）、B 得 405，语义不一致。
- 修复：A 的 GET `/stt` 在非 Upgrade 时返回 405。

### A5-38 [P2] STT 不支持的 Content-Type：A 400 对 B 415
- 本项目：`internal/grok/handler_voice.go:361` — `writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())`（`/stt` 与 `/audio/transcriptions` 用 `mime.ParseMediaType` 白名单，仅 `application/json`、`multipart/form-data`，其余含空类型都走到该分支，handler_voice.go:688-690）
- grok2api：`B:backend/internal/transport/http/inference/voice_handler.go:266` — `writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "STT 支持 multipart/form-data 或 application/json")`
- 差异/错误：同一类"媒体类型不支持"的请求，A 回 400，B 回 415。
- 影响：客户端无法区分"媒体类型不支持"与"请求体非法"，降级/重试策略在 A 上错误。
- 修复：A 对不支持的 Content-Type 返回 415。

### A5-39 [P2] STT multipart 的 `sample_rate_hertz` 未归一化为 `sample_rate`
- 本项目：`internal/grok/handler_voice.go:687` — `return model, hasInput, body, contentType, nil`（multipart 分支把客户端 body 原样转发给 Console；只有 JSON 分支重建 multipart，且白名单 handler_voice.go:637 内无 `sample_rate_hertz`）
- grok2api：`B:backend/internal/transport/http/inference/voice_handler.go:163` — `input.SampleRate = firstNonEmpty(get("sample_rate"), get("sample_rate_hertz"))`（adapter 只重建已知字段，console/voice.go:200-201）
- 差异/错误：B 显式接受 OpenAI 旧字段 `sample_rate_hertz` 作为 `sample_rate` 别名；A 的 multipart 直通路径不识别该字段，也不过滤未知字段。
- 影响：使用 `sample_rate_hertz`（如 16k）的客户端在 A 上该参数被 Console 忽略、按默认采样率转写，结果与 B 不同；未知字段在 A 上直接送上游。
- 修复：multipart 分支同样解析并重建字段表，加入 `sample_rate_hertz`→`sample_rate` 映射。

### A5-40 [P2] TTS/STT 无账号级重试/故障转移
- 本项目：`internal/grok/handler_voice.go:693-701` — `func (h *Handler) forwardConsoleVoice(...)` / `resp, sess, err := h.doConsoleVoice(r, modelID, method, path, body, headers)` / `if err != nil { writeConsoleVoiceRequestError(w, err); return }`
- grok2api：`B:backend/internal/application/gateway/voice.go:454` — `if response.StatusCode >= http.StatusInternalServerError && attemptPolicy.hasNext(attempt) {`（402/429 分支 voice.go:440-453）
- 差异/错误：A 只调用一次 `doConsoleVoice`（仅底层 DPoP/egress 层可能重试一次），429/402/5xx 立即返回错误；B 在 executeVoice 循环里对 402/429 与 >=500 释放租约后换账号重试。
- 影响：单账号被限流或上游 5xx 时 A 直接把错误抛给客户端，可用性低于 B。
- 修复：voice 路径接入账号重试循环。

### A5-41 [P3] 转录"不支持参数"的错误 code/message 不同
- 本项目：`internal/grok/handler_voice.go:461` — `return "", false, nil, "", "", fmt.Errorf("prompt is not supported by Console STT")`（最终渲染 code=`invalid_request`）
- grok2api：`B:backend/internal/transport/http/inference/voice_handler.go:271` — `writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", unsupportedOpenAIParameter+" 暂不支持，不能无损转换到 Console STT")`
- 差异/错误：prompt/temperature/timestamp_granularities 被判不支持时，A 是 `invalid_request` + 英文文案，B 是 `unsupported_parameter` + 中文文案 + `param: null`。
- 影响：客户端无法按 code 区分"参数不支持可降级"与"请求非法"。
- 修复：改用 `unsupported_parameter` 并统一文案。

### A5-42 [P3] multipart 不带 `[]` 的 `timestamp_granularities`：A 拒绝、B 静默忽略
- 本项目：`internal/grok/handler_voice.go:538` — `case "timestamp_granularities", "timestamp_granularities[]":`（两种拼写都判不支持并 400）
- grok2api：`B:backend/internal/transport/http/inference/voice_handler.go:173` — `if form != nil && len(form.Value["timestamp_granularities[]"]) > 0 {`
- 差异/错误：B 只识别带 `[]` 的字段名，不带 `[]` 时既不报错也不透传（STTInput 无该字段），请求成功但选项被静默丢弃；A 两种拼写都显式 400。
- 影响：同一 multipart 请求 A=400、B=200；B 侧存在静默忽略。
- 修复：双方统一为识别该参数并返回显式 400 `unsupported_parameter`，不做静默丢弃。

### A5-43 [P3] voice 请求体上限 64 MiB 对 B 的 32 MiB
- 本项目：`internal/grok/handler_voice.go:19` — `maxVoiceRequestBytes  = 64 << 20`（消费点 handler_voice.go:204/242/352/385 `http.MaxBytesReader(w, r.Body, maxVoiceRequestBytes)`）
- grok2api：`B:backend/internal/infra/config/config.go:877` — `MaxBodyBytes:          32 << 20,`（inference handler 复用该值：handler.go:77，voice_handler.go:32）
- 差异/错误：A 对 `/tts`、`/stt`、`/audio/*` 使用独立的 64 MiB 上限，B 走全局默认 32 MiB。
- 影响：32–64 MiB 的音频在 A 成功、在 B 被拒，同一客户端在两套实现上的最大可上传音频不一致。
- 修复：统一或做成可配置项，默认对齐 32 MiB。

### A5-44 [P3] 语音转发不返回上游 `Retry-After`（与媒体路径一致缺失）
- 本项目：`internal/grok/handler_voice.go:746` — `for _, key := range []string{"Content-Type", "Content-Disposition", "X-Request-Id"} {`（白名单不含 Retry-After）
- grok2api：`B:backend/internal/transport/http/inference/handler.go:521-529` — `func setSafeMediaResponseHeaders(destination, source http.Header) {` … 仅回传 Retry-After 与 X-Request-Id
- 差异/错误：B 的媒体/音频响应会回传上游 `Retry-After`（并过滤其余头）；A 的 voice 头白名单只有 Content-Type/Content-Disposition/X-Request-Id，429 的退避信息丢失（该点与 A5-32 同源，此处单列以便排期）。
- 影响：客户端在上游限流时无法按上游要求退避，只能固定间隔重试，加剧限流。
- 修复：A 的语音头白名单加入 `Retry-After`，并对齐 B 的过滤集合。

### A5-45 [P2] 视频内容下载整段读入内存后返回，B 用可寻址流 + Range/ETag
- 本项目：`internal/grok/handler_videos.go:554` — `data, err := os.ReadFile(job.ContentPath)`（559-560 行 `w.Header().Set("Content-Type", "video/mp4")` / `http.ServeContent(w, r, videoID+".mp4", time.Now(), bytes.NewReader(data))`）
- grok2api：`B:backend/internal/transport/http/media/handler.go:97-107` — `seeker, ok := body.(io.ReadSeeker)` / `c.Header("Content-Type", asset.MIMEType)` / `c.Header("Content-Disposition", mediafile.VideoContentDisposition(asset.ID, asset.MIMEType))` / `c.Header("ETag", ...)` / `http.ServeContent(c.Writer, c.Request, asset.ID, asset.CreatedAt, seeker)`
- 差异/错误：B 从资产存储拿到 `io.ReadSeeker` 直接交给 `http.ServeContent`，内存占用与文件大小无关，并带 Content-Disposition、ETag、nosniff，MIME 取自资产元数据。A 先 `os.ReadFile` 把整个文件读进内存再包成 `bytes.Reader`（A 允许落盘到 512 MiB，见 A5-25），Content-Type 写死 `video/mp4`，且无 Content-Disposition/ETag/nosniff。
- 影响：并发下载大视频时 A 的内存占用按 文件大小 × 并发数 放大，少量请求即可打满内存；客户端也拿不到断点续传所需的 ETag/Content-Disposition。
- 修复：改为 `os.Open` + `http.ServeContent` 流式返回，补 Content-Disposition/ETag/nosniff，并按实际 mime 设置 Content-Type。

## 待验证

- A5-7 中"`grok-imagine-image-quality` 在 B 具体落到 Console 路由"依赖 B 的模型路由表装配（`modeldomain.Route` + `models.GetByPublicIDCandidates`）在运行期合并 web/console 目录的次序；本次只核对了两侧静态目录与别名定义，未做端到端路由复现。`[待验证]`
- A5-22 的 SSRF 结论基于 A 代码中不存在 IP 校验（已穷举 `fetchRemoteAsDataURI`、`isRemoteURL`、`uploadSingleInput` 调用链）；未在运行实例上实际发起内网请求验证。`[待验证]`
- A5-12 中"仅配 Web 账号的部署在 A 必然 503"依赖账号池装配；已核对 A 的取号函数与 B 的 provider 收集逻辑，未做双实例对照实验。`[待验证]`
