# 上下文长度限制点审计

审计范围：`internal/**`、`cmd/**`（排除 `.upstream/`、`.gocache/`、`*_test.go`）
目标：找出代理侧任何会限制、截断或误报上下文窗口的地方。

---

## 0. 修复状态

全部条目已修复。改动概览见文末 §7；本报告正文保留的是**修复前**的原始观测。

---

## 0.1 结论速览（修复前）

| 症状 | 根因 | 位置 |
|---|---|---|
| Warp 通道「很少上下文就忘事/断掉」 | 无状态转录硬截断 **48 KB 字符** | `internal/warp/request.go:38,129` |
| Warp 通道上游报 `context_window_exceeded` | 显式下发 `base_model_context_window_limit = 0` | `internal/warp/request.go:599,628` |
| 客户端（Codex / pi-ai / Claude Code）误判 `context overflow`、过早自动压缩 | `GET /v1/models` **不输出任何窗口字段**，客户端回退默认 262144 或 128000 | `internal/handler/models.go:20-26` |
| 非 Grok 模型被当成 128000 窗口 | codex catalog 默认兜底 128000 | `internal/handler/codex_models.go:114` |
| Qoder / WorkBuddy 窗口不可达 | 账号快照只存模型 ID，丢掉 `max_input_tokens` | `internal/qoder/catalog.go`、`internal/store/store.go:147` |

**关键事实：代理侧不存在任何按 token 计算的历史裁剪或自动压缩。**
`puter` / `workbuddy` / `qoder` 三个通道把客户端 `messages` **全量透传**给上游。
唯一主动丢弃历史的地方就是 Warp 那条 48 KB 字符上限。

> **关于 `base_model_context_window_limit = 0` 的更正**：该 proto 字段自带的注释是
> *"User-selected max input-token context window for the base model. Zero or unset means
> 'use the model's default max'."*（`request.pb.go`）。也就是说发 0 按上游文档语义等价于
> 「不指定」，并非"窗口=0"。它**不是**已被证实的故障源，只是把一个已知的数字交给了上游去猜。
> 修复仍然做了，因为它让窗口由代理显式声明、与 `/v1/models` 对外发布的值保持一致。

---

## 1. Warp：48 KB 无状态转录截断（最严重）

`internal/warp/request.go:35-38`

```go
// A request without a server-issued Warp conversation ID is stateless at the
// upstream. Keep enough transcript to preserve OpenAI/Claude multi-turn
// semantics instead of silently forwarding only the last user message.
const warpStatelessHistoryMaxChars = 48 * 1024
```

`internal/warp/request.go:101-145` `renderWarpStatelessTranscript`：

```go
for i := len(parts) - 1; i >= start; i-- {
    part := parts[i]
    if used+len(part)+2 > warpStatelessHistoryMaxChars && len(selected) > 0 {
        break            // ← 从最新往回装，装满 48 KB 就停
    }
    selected = append(selected, part)
    used += len(part) + 2
}
...
if len(selected) < len(parts)-start {
    selected = append([]string{"[Earlier conversation omitted for length]"}, selected...)
}
```

**触发条件**（`internal/warp/request.go:89-99`）：

```go
func buildWarpUserQuery(promptText string, ..., conversationID string) string {
    if query := strings.TrimSpace(promptText); query != "" { return query }
    if shouldSendWarpConversationID(conversationID) {
        return latestWarpUserInput(messages)   // 有会话 ID：只发最后一条，历史靠上游
    }
    return renderWarpStatelessTranscript(messages, systemItems)  // 无会话 ID：48 KB 截断
}
```

`shouldSendWarpConversationID`（`:665-671`）只认**上游签发**的会话 ID，本地 `chat_` 前缀的一律不算：

```go
return !strings.HasPrefix(conversationID, "chat_")
```

而 `internal/handler/handler.go:953-954` 正是生成 `chat_` 占位符：

```go
if chatSessionID == "" && !isWarpRequest {
    chatSessionID = "chat_" + randomSessionID()
}
```

**48 KB 字符 ≈ 12k~16k token**（中文按 1.5 token/字更少）。对 1M 模型来说，等于用了 1.5% 就把历史扔了。

**放大因素：会话 TTL 只有 30 分钟**
`cmd/server/main.go:203`

```go
sessionStore := handler.NewRedisSessionStore(redisClient, s.RedisPrefix(), 30*time.Minute)
```

`internal/handler/session_store.go:54,87` 的 `Touch` 只在有请求时续期。
一次超过 30 分钟的空档 → 会话 ID 失效 → 下一轮走无状态分支 → 48 KB 截断。

---

## 2. Warp：显式下发 `base_model_context_window_limit = 0`

`internal/warp/request.go:599`

```go
contextLimit := uint32(0)
```

`internal/warp/request.go:626-629`

```go
ModelConfig: warpapi.Request_Settings_ModelConfig_builder{
    Base:                        stringPtr(normalizeWarpModel(req.Model)),
    CliAgent:                    stringPtr(cliAgentModel),
    ComputerUseAgent:            stringPtr(computerAgentModel),
    BaseModelContextWindowLimit: &contextLimit,
}.Build(),
```

该 proto 字段是**显式 presence**（`request.pb.go:7735` + `XXX_presence`，有 `HasBaseModelContextWindowLimit()`），
所以 `0` **会被真正序列化到线上**，不是"省略"。

`internal/warp/UPSTREAM_NOTES.md:88` 也把它写进了"当前请求形状"：

```
- `settings.model_config.base_model_context_window_limit = 0`.
```

**讽刺的是：代理已经解析出了真实窗口却不用。**
`internal/warp/model_choices.go:21,24,208-212,232-235,300`：

```go
type ModelContextWindow struct { Min, Max, Default int }   // 例：min 1024 / max 200000 / default 128000
...
normalized.ContextWindow = ModelContextWindow{
    Min:     choice.ContextWindow.Min,
    Max:     choice.ContextWindow.Max,
    Default: choice.ContextWindow.Default,
}
```

全仓库搜索确认 `ContextWindow` **只在 `model_choices.go` 内部赋值，没有任何消费点**（除测试）。
上游目录告诉你 max=200000，你却发 0。

---

## 3. `GET /v1/models` 不输出上下文窗口（影响所有客户端）

`internal/handler/models.go:18-26`

```go
type PublicModelResponse struct {
    ID            string   `json:"id"`
    Object        string   `json:"object"`
    Created       int64    `json:"created"`
    OwnedBy       string   `json:"owned_by"`
    Capabilities  []string `json:"capabilities,omitempty"`
    Provider      string   `json:"provider,omitempty"`
    UpstreamModel string   `json:"upstream_model,omitempty"`
    // ← 没有 context_length / context_window / max_input_tokens / max_output_tokens
}
```

`publicModelResponse()`（`:38-44`）也只填四个字段。
`HandleModelByID`（`:247+`）走同一个构造函数，所以 `/v1/models/{id}` 同样没有。

**后果链**（见 `docs/diag-analysis-2026-09-19.md:83-101`，已诊断过但未修）：

```
客户端 readListing(): contextWindow = capacity(contextWindow, context_window,
                                               context_length, max_input_tokens, limit.context)
   → 全部取不到 → DEFAULT_CONTEXT_WINDOW = 262144
   → 静默溢出判定：stopReason == "stop" && usage.input + cacheRead > 262144
   → "pi-ai detected context overflow for model XXX" / "本轮运行失败"
```

注意方向问题：**报小了会误判溢出（当前症状），报大了会在上游被截断。** 必须按真实值填。

**唯一带窗口的出口是 Codex 分支，且只对 Grok 模型准。**
`internal/handler/models.go:224-228`

```go
// Codex-family clients ask for a richer catalog that carries the context window...
if strings.TrimSpace(r.URL.Query().Get("client_version")) != "" {
    writeCodexModelCatalog(w, r, newCodexModelCatalog(publicModels))
    return
}
```

`internal/handler/codex_models.go:93-115` 的表只覆盖 Grok：

```go
var codexModelMetadataTable = map[string]codexModelMetadata{
    "grok-4.5":                     {500000, ...},
    "grok-4.6":                     {500000, ...},
    "grok-4.3":                     {1000000, ...},
    "grok-build-0.1":               {256000, ...},
    "grok-4.20-0309-reasoning":     {2000000, ...},
    "grok-4.20-0309-non-reasoning": {2000000, ...},
    "grok-4.20-multi-agent-0309":   {2000000, ...},
    "grok-3-mini":                  {131072, ...},
    "grok-3-mini-fast":             {131072, ...},
    "grok-composer-2.5-fast":       {200000, ...},
}

var codexDefaultMetadata = codexModelMetadata{
    contextWindow: 128000,      // ← 非 Grok 模型（qoder / workbuddy / puter / warp）一律 128000
    description:   codexDefaultDescription,
}
```

`:322-324` 输出：

```go
ContextWindow:                 metadata.contextWindow,
MaxContextWindow:              metadata.contextWindow,
EffectiveContextWindowPercent: 95,
```

对 1M 的 Qoder `ultimate` / `performance` / `dfmodel` 来说，这等于**报小了 8 倍**。

另外 `internal/handler/codex_models.go:319` 还下发：

```go
TruncationPolicy: codexTruncationPolicy{Mode: "tokens", Limit: 10000},
```

告诉 Codex 客户端按 **10000 token** 截断——对 1M 窗口是极保守值（这是对齐 grok2api 上游的行为，`.upstream/grok2api/.../codex_models.go:181` 同样如此）。

---

## 4. Qoder：`context_length` 可能缺失

`internal/qoder/request.go:169-173`

```go
parameters := map[string]interface{}{}
if model.MaxInputTokens > 0 {
    parameters["context_length"] = model.MaxInputTokens
}
```

`MaxInputTokens` 来自账号快照 `qoder_model_ids`。`internal/qoder/catalog.go:188-217` 兼容两种格式：

```go
if strings.HasPrefix(trimmed, "{") {
    var entry modelEntry
    if err := json.Unmarshal([]byte(trimmed), &entry); err != nil { continue }
    entries = append(entries, entry)      // ✅ 新格式：带 max_input_tokens
    continue
}
key, name, _ := strings.Cut(trimmed, "\t")
entries = append(entries, modelEntry{Key: key, Name: name})   // ⚠️ 旧格式：MaxInputTokens = 0
```

旧格式快照 → `MaxInputTokens = 0` → 请求不带 `context_length` → 上游按自己的默认（通常更小）处理。

`internal/qoder/catalog.go:225-227` 的注释已经点明了这点：

```go
// ... a two-field snapshot would silently drop them and the gateway would receive a
// request with no context length.
```

**验证**（真实窗口，来自 `docs/diag-analysis-2026-09-19.md:107-118`）：

| Qoder key | 名称 | max_input_tokens |
|---|---|---|
| `qfmodel` | Qwen3.8-Flash | 180000 |
| `qmodel_38max` | Qwen3.8-Max | 180000 |
| `efficient` / `cmodel` | Efficient / Cantus | 200000 |
| `ultimate` / `performance` | Ultimate / Performance | **1000000** |
| `dfmodel` | DeepSeek-Flash | **1000000** |

检查线上快照格式：

```bash
redis-cli --scan --pattern 'orchids:accounts:id:*' | while read k; do
  redis-cli HGET "$k" qoder_model_ids | head -c 200; echo " <= $k"
done
```

若输出是 `qfmodel\tQwen3.8-Flash` 这种 tab 分隔（而非 `{...}` JSON），就是旧格式，需要刷新模型目录。

---

## 5. 工具侧截断（次要，但长会话会累积）

| 位置 | 常量 | 值 |
|---|---|---|
| `internal/util/tool_results.go:10,37-38` | `PersistedToolResultMaxBytes` | 256 KB / 条工具输出 |
| `internal/warp/request.go:752-754` | `maxWarpToolCount` / `maxWarpToolDescLen` / `maxWarpToolSchemaJSONLen` | 32 个 / 512 字符 / 4 KB |
| `internal/handler/tool_compaction.go:15-17` | `maxCompactToolCount` / `maxCompactToolSchemaJSONLen` / `maxIncomingToolDescLen` | 24 个 / 4 KB / 128 字符 |

注意 handler 层会把工具描述砍到 **128 字符**（`tool_compaction.go:278-281`），Warp 层再砍到 512。

---

## 6. 已排除：不是限制的地方

| 位置 | 值 | 为什么不是问题 |
|---|---|---|
| `internal/handler/handler.go:130` | `maxRequestBytes = 50 MB` | 1M token 文本约 3~4 MB，远低于上限 |
| `internal/grok/grok2api_sse.go:14` | `upstreamMaxEventBytes = 8 MB` | 单 SSE 事件上限，非总量 |
| `internal/puter/stream.go:55` | scanner 4 MB | 同上 |
| `internal/workbuddy/stream.go:136` | scanner 8 MB | 同上 |
| `internal/qoder/stream.go:201` | scanner 16 MB | 同上 |
| `internal/config/config.go` | ~~`WarpMaxToolResults=10` / `WarpMaxHistoryMessages=20`~~ | **已删除**：只参与 client cache key 哈希（`client_cache.go`），全仓库无实际消费点。见 §7 补充 |
| `internal/handler/caching.go` | prompt caching | 只加 `cache_control` 标记，不删内容 |
| `internal/handler/system_sanitize.go` | `ccEntrypointModeKeep` | 保真透传，不改写 |
| `internal/grok/responses_compaction.go` | Grok 压缩 | 只在客户端显式请求 `/responses/compact` 时触发，不自动裁输入 |
| `internal/handler/handler.go` / `token_breakdown.go` | token 估算 | 仅用于 usage 上报与 `count_tokens`，**不参与任何门禁判断**（但见 §7 补充：它此前按压缩投影少报） |
| `internal/middleware/billing.go:31` | `maxBillingBodyBytes = 8 MB` | 超限时 `MultiReader` 全量回放给 handler，只截断计费估算用的前缀 |

---

## 7. 已实施的修复

### P0-1：`/v1/models` 输出观测到的窗口 ✅

- `internal/handler/models.go` — `PublicModelResponse` 新增 `context_length` / `max_input_tokens` / `max_output_tokens`；`HandleModels` 与 `HandleModelByID` 都填值。
- 新增 `internal/handler/model_context.go` — `observedModelContextWindows()` 一次读取所有渠道已观测的窗口，`modelContextWindow()` 逐行解析。
- **未观测到的模型不输出这些字段**（`omitempty`），而不是输出 0。

数据来源（全部是已存在的观测，不发明数字）：

| 渠道 | 来源 |
|---|---|
| Qoder | 账号快照 `qoder_model_ids[]` → 新增 `qoder.CatalogContextWindows()` |
| WorkBuddy | 账号快照 `workbuddy_model_ids[]` → 新增 `internal/workbuddy/catalog.go` |
| Warp | 发现缓存的 `context_windows`（上游 `contextWindow.max`） |
| Grok | `codexModelMetadataTable` |
| Puter | 无可信来源 → 不输出 |

### P0-2：Warp 显式声明已解析的窗口 ✅

`internal/warp/request.go` — 只在 `WarpContextWindowLimit > 0` 时设置 `base_model_context_window_limit`；
未解析到时**不设置该字段**（proto 语义即"用模型自身的默认上限"）。
窗口经 `upstream.UpstreamRequest.WarpContextWindowLimit` 传入，由 `handler.resolveWarpRequestFeatures()` 从
`warp.AccountModelChoices.ContextWindows` 解析，并在换号/重试时同步刷新。

### P0-3：抬高传输上限 + 延长会话绑定 ✅

- `warp.StatelessHistoryMaxChars`：48 KiB → **8 MiB**（可用 `warp_stateless_history_max_chars` 配置，上限 64 MiB）
- 会话绑定 TTL：30 min → **12 h**（可用 `session_ttl_minutes` 配置，上限 30 天）
  - `cmd/server/main.go`（Redis）与 `handler.NewWithLoadBalancer`（内存）都改为读配置

### P1：Codex catalog 优先使用观测窗口 ✅

`internal/handler/codex_models.go` — `item.ContextLength > 0` 时覆盖静态表/128000 兜底。
Grok 模型行为不变（仍走原表），未观测到的模型仍用 128000 兜底，避免把"未知"变成 0。

### P1：模型快照持久化窗口 ✅

- `internal/workbuddy/catalog.go`（新增）— `CatalogSnapshot()` 写 JSON 行，`CatalogContextWindows()` 读回；**兼容旧的裸 ID 行**。
- 写入点全部改为 `CatalogSnapshot()`：`cmd/server/model_refresh.go`、`internal/api/api_workbuddy.go`、`internal/api/api_workbuddy_login.go`。
- Warp：`AccountModelChoices` 新增 `context_windows`，在 `model_refresh`、`account_refresh`、`refreshWarpModelConfigAsync` 三处合并写入；`saveWarpAccountModelChoices` 改为先读后合并，避免整表覆盖时丢掉其它账号观测到的窗口。
- Qoder 快照本就走 `CatalogSnapshot()`（JSON 行）；旧格式仍可读，刷新一次即升级。

### 补充：工具面保真 ✅

前一轮把这两项列为"未改动"，本轮一并处理。

**1. 删除死配置 `WarpMaxToolResults` / `WarpMaxHistoryMessages`**

它们唯一读者是 account client 缓存键（`internal/handler/client_cache.go`），从不裁剪任何内容。
一个"看起来能省上下文"的旋钮实际什么都不做，比没有更危险——运维调低它，既无收益也无告警。
字段、`ApplyHardcoded` 默认值、缓存键写入全部删除。`TestWarpPassthrough_DoesNotTrimMessagesOrSanitizeSystem`
改为直接守护"透传"行为本身，不再依赖两个惰性字段。

**2. 工具 token 估算改为按实际发送的定义计算**

`internal/handler/tool_compaction.go` 的 `compactIncomingTools` 是一条**估算专用**路径：
它按"最多 24 个工具 / 描述截到 128 字符 / schema 截到 4 KiB"投影后再计数，
而真正发给上游的是 `effectiveTools = req.Tools`（逐字）。也就是说它描述的是一个从不存在的请求，
并且**少报**输入 token —— `count_tokens` 与前置 usage 都取这个数字。现改为 `estimateToolsTokens()`
直接度量实际转发的定义；约 200 行不再被任何生产路径调用的压缩代码及其白名单已删除。

**3. Warp 工具定义停止被改写**

`internal/warp/request.go` 的 `convertTools` 会真正改写发往上游的 payload：

| 旧行为 | 后果 | 现在 |
|---|---|---|
| 内置工具按硬编码白名单过滤 properties | 客户端声明的参数被静默删除（如 Bash 的 `dangerouslyDisableSandbox`） | 逐字透传 |
| schema 只保留 `{type,description,properties,required,enum,items}` | `additionalProperties` / `oneOf` / `$schema` / `pattern` / `default` 丢失，严格 schema 失效 | 逐字透传 |
| schema > 4 KiB 直接替换成空对象 | 大工具变成"不接收任何参数" | 逐字透传 |
| 描述截到 512 字符 | 删掉的正是"何时该用/何时会失败"的说明 | 上限 64 KiB，仅作传输保护 |
| 工具数上限 32 | 第 32 个之后的工具对模型不可见 | 上限 256，仅作传输保护 |

### 新增测试

`internal/handler/model_context_test.go`、`internal/warp/context_window_test.go`、
`internal/warp/tool_fidelity_test.go`、`internal/workbuddy/catalog_test.go`、
`internal/qoder/context_window_test.go`、`internal/config/config_test.go`。

### 仍未改动（已确认，非缺陷）

- `internal/handler/tool_compaction.go` 的 `passthroughAllowedToolNames(tools, supportedOnly=true)`
  分支在生产路径不可达（唯一调用点传 `false`），仅测试使用。它只产出"允许的工具名"清单用于
  校验模型的工具调用，不影响发给上游的内容。保留是因为删除它属于另一类清理，与本报告范围无关。

---

## 8. 验证命令

```bash
# 1. 看 /v1/models 的窗口字段（修复后：有值；未观测到则字段缺失）
curl -s -H "Authorization: Bearer $KEY" http://HOST:3002/qoder/v1/models | jq '.data[0]'

# 2. 看 Codex 客户端拿到的窗口（带 client_version）
curl -s -H "Authorization: Bearer $KEY" \
  "http://HOST:3002/v1/models?client_version=0.9.0" | jq '.models[] | {slug, context_window, truncation_policy}'

# 3. 看快照格式（修复后应为 JSON 行；裸 ID / key\tname 为升级前遗留）
redis-cli --scan --pattern 'orchids:accounts:id:*' | while read k; do
  echo "== $k"; redis-cli HGET "$k" qoder_model_ids | head -c 300; echo
  redis-cli HGET "$k" workbuddy_model_ids | head -c 300; echo
done

# 4. 看 Warp 发现缓存里的窗口表
redis-cli GET 'orchids:settings:warp_account_model_choices' | jq '.context_windows'

# 5. 看本轮实际输入 token（summary 里的 input_tokens 是上游原样转发，可信）
curl -s -H "Authorization: Bearer $KEY" -X POST http://HOST:3002/qoder/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"qwen3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' \
  | jq '.usage'
```

> 升级后需要**各渠道刷新一次模型目录**（`/api/models/refresh`），让窗口写进账号快照；
> Warp 的 `context_windows` 在下一次模型发现（刷新或请求触发的 stale-config 刷新）后出现。
> 在刷新之前，未观测到的模型会保持"不输出窗口字段"，不会输出错误数字。

---

## 9. 附：现存文档交叉引用

- `docs/diag-analysis-2026-09-19.md:83-101` — 已诊断"上下文窗口不外泄 → 客户端 262144 误判溢出"（本次已修）
- `docs/diag-analysis-2026-09-19.md:317-356` — 附录 A「把上下文窗口暴露给客户端的代理侧最小改法」，**已按 P0-1 实施**
- `docs/configuration.md:146` — `context_max_tokens`：「旧兼容字段；不在中转层截断或自动压缩上下文」（与本报告"无 token 级裁剪"结论一致）
- `docs/configuration.md:3.3` — 新增的 `session_ttl_minutes` / `warp_stateless_history_max_chars` 与窗口字段说明
- `internal/warp/UPSTREAM_NOTES.md:88` — 记录 `base_model_context_window_limit = 0`（本次已改为按需显式声明）

