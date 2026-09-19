# 用量统计 / 计费 / 缓存对比

审计范围：A（本项目，Orchids-2api，移植 chenyme/grok2api 44a390b8）vs B（.upstream/grok2api HEAD 906b9493, v3.1.6）。
以下 B 路径相对 `backend/`，并统一加 `B:` 前缀。

## 结论摘要

A 把 B 的 **用量采集（token 计数）** 这一层搬了过来，但 **没有搬计费层**：A 全仓 `cost_in_usd_ticks` / `EstimateOfficialCost` / `ReserveBilling` / `usage_source` 命中数为 **0**（`grep -rn "cost_in_usd_ticks\|CostInUSDTicks\|CostInUSD\|EstimatedCost\|ReserveBilling\|usage_source" internal/` 无输出），而 B 有完整的官方价格表、USD-tick 成本计算、Key 计费预留、`usage_source` 列与 priced/unpriced 计费维度。因此 A 可以"统计 token"，但**无法对任何 Key 扣费、无法拦截超支、无法在 journal 里还原成本**（P0）。

其余为映射/口径差异：A 与 B 在同一语义点上给出不同的数字（上游 usage 字段被重建后丢弃 cost/上下文明细；Anthropic usage 缺 thinking/cache 维度；estimator 覆盖范围不同；prompt cache key 派生信号集不同；本地额度扣减缺失）。

关于 19 个 commit 的漂移：B 新增的 `B:internal/infra/provider/conversation/reasoning_cache.go`（Build 工具调用 ↔ reasoning 证明的按 call_id 作用域缓存，4096 条 / 30min）在 A 中完全没有对应物（见 A9-13），属**漂移导致的缺口**；其余发现均为移植期就存在的口径差异。

| 编号 | 级别 | 一句话 |
| --- | --- | --- |
| A9-1 | P0 | 无价格表/无成本字段/无 Key 计费预留与结算 |
| A9-2 | P1 | journal 不记录 usage 来源（上游报告 vs 本地估算） |
| A9-3 | P1 | 归一化上游 usage 时丢弃 cost_in_usd_ticks 等字段 |
| A9-4 | P1 | Anthropic usage 缺 thinking_tokens，cache_read 条件性缺省 |
| A9-5 | P1 | `message_start` usage 恒为 0（缺 input/cache 维度） |
| A9-6 | P1 | prompt cache key 信号集不同 + 无标题请求排除 |
| A9-7 | P1 | 无本地额度扣减与 selector 配额消费 |
| A9-8 | P1 | Build billing 百分比不反推，呈现优先级与 B 相反 |
| A9-9 | P2 | 工具调用 completion token 估算把 JSON 结构算入 |
| A9-10 | P2 | prompt token 估算范围不同（role/name/id/工具 JSON） |
| A9-11 | P2 | 图片 usage 造数（64×n）vs B 明确为 0/缺省 |
| A9-12 | P2 | 审计/运营聚合缺 cached/reasoning/total 与 priced 维度 |
| A9-13 | P2 | 缺 Build 按 call_id 语义的 reasoning 证明缓存（漂移） |

## 发现

### A9-1 [P0] A 完全没有价格/成本账本与 Key 计费预留，B 对每个响应计价并预留额度

- 本项目：`internal/grok/usage_estimate.go:122` — `return map[string]interface{}{ "prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens, "prompt_tokens_details": ...}`（usage 里只有计数器，没有任何成本字段）
- 本项目：`internal/audit/audit.go:46` — `InputTokens int \`json:"input_tokens,omitempty"\`` / `OutputTokens` / `CachedInputTokens` / `ReasoningTokens`（`Event` 结构没有 cost、没有 pricing_model、没有 usage_source）
- 本项目：`internal/store/store.go:309` — `type ApiKey struct { ID ... RPMLimit int; MaxConcurrent int; ExpiresAt ... }`（ApiKey 无任何额度/计费字段，故 Key 级扣费无处落账）
- grok2api：`B:internal/domain/audit/pricing.go:199` — `cachedTokens := max(int64(0), min(cachedInputTokens, inputTokens))` / `uncachedTokens := max(int64(0), inputTokens-cachedTokens)` / `return PricingResult{Model: price.CanonicalModel, CostInUSDTicks: uncachedTokens*inputPrice + cachedTokens*cachedPrice + outputTokens*outputPrice}, true`
- grok2api：`B:internal/application/gateway/service.go:1103` — `tokenPricing, tokenPriced := audit.EstimateOfficialCost(pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens)`；预留：`B:internal/application/gateway/service.go:1030` — `if reservation, priced := audit.EstimateOfficialTextReservation(pricingModel, input.Body); priced {` / `service.go:1031` — `if _, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, s.textBillingReservationTTL()); err != nil {`
- grok2api：`B:internal/domain/audit/pricing.go:202` 的费率来自 `buildOfficialTokenPrices()`（如 `grok-build-0.1` 输入 10000 / 缓存 2000 / 输出 20000 ticks per token，长上下文档 20000/4000/40000），并有 `PricingUnitToken/Image/Second` 分档。
- 差异/错误：A 既没有官方费率表，也没有 `cost_in_usd_ticks` 字段、没有 `ReserveBilling`/结算入口；A 的 `internal/audit.Event` 与 journal 行都不存在成本列。B 对同一份 token 数用 `uncached*inputPrice + cached*cachedPrice + output*outputPrice` 得出 USD-ticks，写入 audit 行的 `EstimatedCostInUSDTicks`/`PricingModel`/`PricingVersion`，并在请求前按 `EstimateOfficialTextReservation` 预留。
- 影响：这是钱的问题——A 无法对 API Key 做额度扣减或超支拦截（`ApiKey` 里压根没有额度字段），admin/journal 也无法回答"这次请求花了多少钱/哪个 Key 快超支"，而 A 本身是带 Key 的多渠道代理。
- 修复：移植 B 的 `domain/audit/pricing.go`（费率表 + `EstimateOfficialCost` + 长上下文档 + 图片/视频/语音档），在 A 的 `audit.Event` 增加 `EstimatedCostInUSDTicks/PricingModel/PricingVersion`，并在 `ApiKey` 上加额度字段 + 请求前预留/完成后结算（对齐 `ReserveBilling`）。

### A9-2 [P1] A 不记录 usage 来源，估算值与上游报告值在 journal 中不可区分

- 本项目：`internal/grok/console.go:608` — `outcome.Usage = firstUsage(consoleUsage(raw), addReasoningUsage(buildChatUsagePayload(req, text+refusal, toolCalls), reasoning))`（`firstUsage` 只在第一个 map 为空时才用第二个，见 `console.go:747`；选到哪个分支**不留任何标记**）
- 本项目：`internal/grok/console_stream.go:455` — `if outcome.Usage == nil { outcome.Usage = addReasoningUsage(buildChatUsagePayload(req, text.String()+refusal.String(), calls), reasoning.String()) }`
- 本项目：`internal/audit/audit.go:31` — `type Event struct { Timestamp ...; Kind Kind; RequestID ... }`（无 `usage_source` 类字段，attempt 事件只在 `metadata["usage_reported"]` 里留了一个布尔，见 `attempt_diagnostics.go:175`，而 attempt 行在 journal 列表里不单独成行）
- grok2api：`B:internal/domain/audit/audit.go:24` — `UsageSourceUpstream UsageSource = "upstream"` / `UsageSourceEstimated UsageSource = "estimated"` / `UsageSourceNone UsageSource = "none"`
- grok2api：`B:internal/application/gateway/service.go:947` — `usageSource := audit.UsageSourceUpstream` / `if usageKind, _ := s.providers.UsageKind(route.Provider); usageKind == provider.UsageEstimated { usageSource = audit.UsageSourceEstimated }`；落库校验：`B:internal/infra/persistence/relational/audit_repository.go:113` — `if !auditStringAllowed(row.UsageSource, "upstream", "estimated", "none") { return errors.New("usage_source is invalid") }`
- 差异/错误：B 在 provider 定义层声明权威性（`B:internal/infra/provider/web/definition.go:31` `Usage: provider.UsageEstimated`；`B:internal/infra/provider/cli/definition.go:24` `Usage: provider.UsageUpstream`；`B:internal/infra/provider/console/definition.go:40` `Usage: provider.UsageUpstream`），并把该结论持久化成受约束的列。A 没有这个维度：同一个 grok 账号/模型，一次请求报的是上游数字、下一次报的是 `(runes+3)/4` 估算，journal 里长得完全一样。
- 影响：任何按 journal 做用量/成本回溯的人无法判断某行是"上游账单"还是"本网关猜的"，误差无法归因；也无法像 B 那样把 estimated 行排除在计费之外。
- 修复：在 `audit.Event` 增加 `UsageSource`，在 `consoleUsage`/`buildChatUsagePayload` 两条分支上打标，并按 provider 声明缺省值（与 B 的 `InferencePolicy.Usage` 对齐）。

### A9-3 [P1] 归一化上游 usage 时丢掉 cost_in_usd_ticks / num_sources_used / context_details

- 本项目：`internal/grok/console.go:338` — `return map[string]interface{}{ "prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total, "prompt_tokens_details": map[string]interface{}{ "cached_tokens": cached, "text_tokens": prompt, ...}, "completion_tokens_details": ... }`（从 `raw` 里只取 3 个计数器和两个 details，其余键全部丢弃）
- grok2api：`B:internal/infra/provider/conversation/chat_response.go:48` — `return map[string]any{ "prompt_tokens": value.InputTokens, "completion_tokens": value.OutputTokens, "total_tokens": total, "prompt_tokens_details": map[string]any{"cached_tokens": value.InputTokensDetails.CachedTokens}, "completion_tokens_details": map[string]any{"reasoning_tokens": value.OutputTokensDetails.ReasoningTokens}, "cost_in_usd_ticks": value.CostInUSDTicks, "num_sources_used": value.NumSourcesUsed, "num_server_side_tools_used": value.NumServerSideToolsUsed, "context_details": map[string]any{"input_tokens": value.ContextDetails.InputTokens, "output_tokens": value.ContextDetails.OutputTokens} }`
- 差异/错误：B 的用法映射是"原样透传上游计量"（成本、来源数、服务端工具数、context_details 都保留）；A 的 `consoleUsage` 是"重建一个白名单 map"，上游给的 `cost_in_usd_ticks`、`num_sources_used`、`num_server_side_tools_used`、`context_details` 在 A 侧全部消失（`responsesUsageFromChat` 用 `cloneStringInterfaceMap(usage)` 克隆，但克隆的源已经被 `consoleUsage` 洗过）。
- 影响：Build/Console 平面真实上报的成本与上下文长度在 A 中不可见；下游 Responses/Anthropic 输出也没有成本字段可透传（见 A9-4）。
- 修复：`consoleUsage` 改为保留原 map 的未知键（或显式透传 `cost_in_usd_ticks`/`num_sources_used`/`num_server_side_tools_used`/`context_details`）。

### A9-4 [P1] Anthropic `/v1/messages` usage 缺 `output_tokens_details.thinking_tokens`，`cache_read_input_tokens` 仅在 >0 时才出现

- 本项目：`internal/grok/handler_messages.go:784` — `input := max(0, interfaceToInt(usage["prompt_tokens"]))` / `result := map[string]interface{}{ "input_tokens": input, "output_tokens": interfaceToInt(usage["completion_tokens"]) }` / `if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok { if cached := interfaceToInt(details["cached_tokens"]); cached > 0 { cached = min(cached, input); result["input_tokens"] = input - cached; result["cache_read_input_tokens"] = cached } }`
- grok2api：`B:internal/infra/provider/conversation/messages_response.go:108` — `inputTokens := max(int64(0), value.InputTokens)` / `cacheReadInputTokens := min(inputTokens, max(int64(0), value.InputTokensDetails.CachedTokens))` / `thinkingTokens := min(outputTokens, max(int64(0), value.OutputTokensDetails.ReasoningTokens))` / `usage := map[string]any{ "input_tokens": inputTokens - cacheReadInputTokens, "output_tokens": outputTokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": cacheReadInputTokens, "output_tokens_details": map[string]any{"thinking_tokens": thinkingTokens}, "cost_in_usd_ticks": value.CostInUSDTicks, ... }`
- 差异/错误：① B **无条件**输出 `cache_read_input_tokens`/`cache_creation_input_tokens`（缺省为 0），A 只在 `cached > 0` 时才补键，缺失时客户端字段不存在；② A 没有 `output_tokens_details.thinking_tokens`（B 用 `min(output, reasoning)` 夹紧后作为 output 的分解项）；③ A 没有 `cost_in_usd_ticks`/`num_sources_used`/`context_details`；④ A 的 `output_tokens` 直接取 `completion_tokens` 且不做 `max(0,·)`。
- 影响：Anthropic 客户端（含 Claude Code 的费用/缓存展示）拿不到 thinking 与 cache-write 维度，也无法区分"无缓存"与"字段缺失"。
- 修复：对齐 `anthropicUsage` 的字段集与夹紧算法（含 `min(output, reasoning)`、无条件 cache 字段）。

### A9-5 [P1] A 的 `message_start` usage 恒为 0，B 在 `message_start` 就给出真实 input/cache 数字

- 本项目：`internal/grok/handler_messages.go:841` — `usage: map[string]interface{}{"input_tokens": 0, "output_tokens": 0}`，随后 `handler_messages.go:844` — `writeAnthropicSSE(w, "message_start", ... "usage": state.usage)`（`state.usage` 只有在上游 chat 流的最后一帧才被 `anthropicUsageFromOpenAI` 覆盖，见 `handler_messages.go:869`）
- grok2api：`B:internal/infra/provider/conversation/messages_stream.go:9` — `usage := anthropicUsage(c.usage, 0)` / `delete(usage, "output_tokens_details")` / `"usage": usage`；web 通道同语义：`B:internal/infra/provider/web/chat.go:2166` — `"usage": map[string]any{"input_tokens": s.inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}`
- 差异/错误：Anthropic 语义要求 `message_start` 携带 input_tokens 与 cache 计数；A 的翻译器在流开始时还不知道 input（它只在上游 chat 的 finish 帧里读到 usage），于是一律写 `input_tokens: 0`。B 在构造响应时就知道 input token（web 用 `estimateTokens`、conversation 用上游 `responseUsage`），因此 `message_start` 给出真实值。
- 影响：依赖 `message_start` 统计输入 token / 缓存命中的客户端（Claude Code 的 `/cost`、代理统计）在 A 上恒为 0；直到 `message_delta` 才被纠正为真实值。
- 修复：在翻译器入口前就确定 input token（A 的 chat 层已经算了 `estimatePromptUsageFromRequest`），用它初始化 `message_start` 的 usage。

### A9-6 [P1] prompt cache key 派生信号集不同：A 缺 Claude Code / Codex 信号、缺 `user_id` 的 `_session_` 解析、也没有标题请求排除

- 本项目：`internal/grok/session_state.go:58` — `for _, header := range []string{"x-grok-session-id", "x-grok-conv-id", "x-session-id", "session-id"} { if seed = strings.TrimSpace(r.Header.Get(header)); seed != "" { break } }`
- 本项目：`internal/grok/handler_messages.go:393` — `func anthropicPromptCacheKey(metadata map[string]interface{}) string { for _, key := range []string{"prompt_cache_key", "session_id", "user_id"} { if value := strings.TrimSpace(fmt.Sprint(metadata[key])); value != "" && value != "<nil>" { return value } } return "" }`（`user_id` 原样当种子，不解析 `_session_` 标记、不排除 Claude Code 的标题生成请求）
- grok2api：`B:internal/transport/http/inference/prompt_cache.go:25` — `if seed := normalizePromptCacheSeed(headers.Get("X-Claude-Code-Session-Id")); seed != "" { return claudeCodePromptCacheSeed(seed, headers) }` / `if seed := codexPromptCacheSeedFromHeaders(headers); seed != "" { return seed }` / 循环 `[]string{"X-Session-Id", "Session-Id", "Session_id", "X-Conversation-Id", ...}` / body 侧 `payload.Metadata.SessionID`、`payload.Metadata.UserID`、`payload.ClientMetadata["x-codex-turn-metadata"]`、`payload.ClientMetadata["x-codex-window-id"]`
- grok2api：`B:internal/transport/http/inference/prompt_cache.go:20` — `if isClaudeCodeTitleRequest(headers, body) { return "" }`（第 19 行注释：`If an auxiliary request uses the explicit session seed, it shares reasoning replay with the main conversation.`）；`B:internal/transport/http/inference/prompt_cache.go:229` — `const marker = "_session_"` / `if index := strings.LastIndex(userID, marker); index >= 0 { return normalizePromptCacheSeed(userID[index+len(marker):]) }`
- 差异/错误：同一份 Claude Code / Codex 客户端流量，B 会得到稳定且带 agent/window 维度的种子（`claude:<sid>:agent:<id>`、`codex:window:<id>`），A 只能拿到 `x-session-id` 或原始 `user_id` 全串（含账号前缀）——键不同即上游 prompt cache 不命中；更严重的是 A 没有 `isClaudeCodeTitleRequest` 之类的排除，标题生成这类旁路请求会与主会话共用同一个 (model, sessionKey) 的 reasoning replay 槽位，把该槽位覆盖成另一轮的 items。
- 影响：`cached_tokens` 长期为 0（上游无缓存命中），且 reasoning replay 可能被辅助请求污染；与 B 的 key 派生不一致会导致 A/B 混跑时缓存与 replay 状态不可迁移。
- 修复：移植 `extractPromptCacheSeed` 全套（含 header 优先级、Codex turn metadata/window、`promptCacheSeedFromUserID`、`isClaudeCodeTitleRequest`），并把 `upstreamID`/`affinityKey`/`replayKey` 三键分离（B 的做法见 `B:internal/application/gateway/prompt_cache.go:47`）。

### A9-7 [P1] A 没有本地额度扣减，B 每成功请求对窗口做 `remaining - units`

- 本项目：`internal/grok/quota.go:122` — `if info.HasRemaining { limit := InferQuotaLimit(acc); if info.HasLimit && info.Limit > 0 { limit = float64(info.Limit) } ... if acc.UsageCurrent != remaining { acc.UsageCurrent = remaining; changed = true } }`（`ApplyQuotaInfo` 只把上游头里的数字**覆盖**进账号，从不做请求级递减）
- 本项目：`internal/grok/quota.go:160` — `func ApplyBuildRateLimits(acc *store.Account, headers http.Header) bool { ... acc.GrokRateLimits = store.GrokRateLimitSnapshot{Requests: requests, Tokens: tokens, ObservedAt: time.Now().UTC()} }`（passive 快照，同样不递减）
- grok2api：`B:internal/application/gateway/service.go:1139` — `if successful && lease.QuotaMode != "" {` / `service.go:1140` — `if lease.QuotaMode != "weekly" { units := max(1, response.QuotaUnits)` / `service.go:1145` — `updated, decrementErr = s.accounts.DecrementQuota(stageCtx, accountID, lease.QuotaMode, units)` / `service.go:1151` — `s.selector.ConsumeQuota(credential.Provider, accountID, lease.QuotaMode, units)`
- grok2api：`B:internal/infra/persistence/relational/account_repository.go:2745` — `"remaining": gorm.Expr("CASE WHEN remaining <= ? THEN 0 ELSE remaining - ? END", amount, amount)`（配合 `account_repository.go:2733` 的 `Where("account_id = ? AND mode = ? AND remaining > 0", ...)`）
- 差异/错误：B 在维护一个本地请求计数窗口：每次成功请求把 `remaining` 减去 `max(1, QuotaUnits)` 并通知 selector 消费（`remaining` 不会变负，递减失败即视为耗尽）；对 `weekly` 明确**不**递减（因为周额度只有上游百分比，见 `lease.QuotaMode != "weekly"`）。A 完全没有这条路径，`Remaining` 只在下一次上游同步时跳变。
- 影响：两次上游同步之间 A 的 `quota_remaining`/`quota_used` 是**陈旧偏高**的，负载均衡也不会因为本地已消耗而改变选择；上游头缺失时 A 甚至完全没有消耗记录。
- 修复：移植 `DecrementQuota`/`ConsumeQuota` 与请求计数窗口（对 web auto/fast 这类"请求单位"窗口递减，对 weekly 百分比窗口保持不递减）。

### A9-8 [P1] Build billing 不做百分比反推，且额度呈现优先级与 B 相反

- 本项目：`internal/grok/cli_billing.go:65` — `if payload.Config.CreditUsagePercent != nil { info.HasUsagePercent = true; info.UsagePercent = *payload.Config.CreditUsagePercent }`（没有 `creditUsagePercent` 时不会从 `monthlyLimit`/`used` 反推；`cli_billing.go:91` 甚至直接报错 `"grok cli billing response contains no weekly quota"`）
- 本项目：`internal/api/quota_projection.go:297` — `if weekly.HasUsage { fields["quota_limit"] = 100.0; fields["quota_used"] = weekly.UsagePercent; fields["quota_remaining"] = max(0, 100-weekly.UsagePercent) ... }`，其后才是 `if monthly.HasLimit { ... if !weekly.HasUsage { ... } }`（percent 窗口**优先**）
- grok2api：`B:internal/infra/provider/cli/billing.go:136` — `if result.CreditUsagePercent == 0 { switch { case result.OnDemandCap > 0: result.CreditUsagePercent = result.OnDemandUsed / result.OnDemandCap * 100; case result.MonthlyLimit > 0: result.CreditUsagePercent = result.Used / result.MonthlyLimit * 100 } }`
- grok2api：`B:internal/application/account/service.go:1185` — `switch { case billing.MonthlyLimit > 0: result.Used = billing.Used; result.Limit = billing.MonthlyLimit; result.Remaining = billing.Remaining(); result.UsagePercent = billing.Used / billing.MonthlyLimit * 100; result.LimitKnown = true; case billing.OnDemandCap > 0: ... result.Used = billing.OnDemandCap * billing.CreditUsagePercent / 100 ...; case billing.PrepaidBalance > 0: ...; case billing.UsagePeriodType != "": result.Unit = "percent"; result.Used = billing.CreditUsagePercent; result.Limit = 100; result.Remaining = max(0, 100-billing.CreditUsagePercent) }`
- 差异/错误：① B 在百分比缺失时用 `used/limit*100`（月度）或 `onDemandUsed/onDemandCap*100` 反推，A 直接放弃（并可能整次误判为"没有周额度"）；② 优先级相反：B 依次取 MonthlyLimit（绝对 credits）→ OnDemandCap → PrepaidBalance → percent；A 先取 percent 周窗口，仅当 `!weekly.HasUsage` 才看月度。同一份 billing 响应，A 展示 `quota_limit=100/quota_used=UsagePercent`（percent 模式），B 展示 `Limit=MonthlyLimit/Used=Used`（credits 模式）。
- 影响：运营看到的"额度/已用"含义与量纲随实现而变；A 在只有月度数字的账号上显示"无额度"，在两者都有时又优先显示百分比窗口。
- 修复：移植 `parseBilling` 的反推逻辑与 `newQuotaView` 的优先级/量纲，并把反推结果写进 `GrokBillingSnapshot.Weekly`。

### A9-9 [P2] 工具调用的 completion token 估算把 JSON 结构算进去，B 只算 name+arguments

- 本项目：`internal/grok/usage_estimate.go:106` — `func estimateCompletionUsage(finalContent string, toolCalls []map[string]interface{}) chatUsageEstimate { var out chatUsageEstimate; out.completionTextTokens += approxTokenCount(finalContent); if len(toolCalls) > 0 { if raw, err := json.Marshal(toolCalls); err == nil { out.completionTextTokens += approxTokenCount(string(raw)) } } return out }`（`toolCalls` 是 OpenAI 形状 `{"id":..,"type":"function","function":{"name":..,"arguments":..}}`，marshaled 后引号/花括号/键名全部计入）
- grok2api：`B:internal/infra/provider/web/chat.go:2120` — `func estimateToolCallTokens(calls []parsedToolCall) int64 { var total int64; for _, call := range calls { total += estimateTokens(call.Name) + estimateTokens(call.Arguments) }; return total }`，调用点 `B:internal/infra/provider/web/chat.go:1800` — `outputTokens := estimateTokens(parsed.Text.String()) + estimateTokens(parsed.Reasoning.String()) + estimateToolCallTokens(parsed.ToolCalls)`
- 差异/错误：A 估算的是"工具的 JSON 序列化体积"（含 ~10+ 个固定结构字符/键名/引号，每个 call 都算一遍），B 估算的是"name 文本 + arguments 文本"。同一次 tool call，A 恒大于 B。
- 影响：工具密集的会话在 A 上 completion_tokens 系统性偏高（进而在 A9-1 修好后会直接多计费）；跨实现对比用量/成本时不可比。
- 修复：改为 `sum(approxTokenCount(name)+approxTokenCount(arguments))`，与 `estimateToolCallTokens` 对齐。

### A9-10 [P2] prompt token 估算范围不同：A 额外计 role/name/tool_call_id 与工具 JSON 结构，B 只计渲染后的 prompt 文本

- 本项目：`internal/grok/usage_estimate.go:40` — `out.promptTextTokens += approxTokenCount(msg.Role); out.promptTextTokens += approxTokenCount(msg.Name); out.promptTextTokens += approxTokenCount(msg.ToolCallID)`；`usage_estimate.go:45` — `for _, tc := range msg.ToolCalls { out.promptTextTokens += approxTokenCount(tc.ID); out.promptTextTokens += approxTokenCount(tc.Type); ... }`；`usage_estimate.go:56` — `if raw, err := json.Marshal(req.Tools); err == nil { out.promptTextTokens += approxTokenCount(string(raw)) }`；`usage_estimate.go:61` — `json.Marshal(req.ToolChoice)`
- grok2api：`B:internal/infra/provider/web/chat.go:247` — `normalized.Prompt = injectToolPrompt(normalized.Prompt, tools)`；`B:internal/infra/provider/web/chat.go:351` — `parsed.InputTokens = estimateTokens(normalized.Prompt)`；公式 `B:internal/infra/provider/web/chat.go:2667` — `count := utf8.RuneCountInString(value); if count == 0 { return 0 }; return int64((count + 3) / 4)`
- 差异/错误：基础公式一致（A `approxTokenCount` = `max(1,(runes+3)/4)`，额外 `TrimSpace`；B 不 trim），但**输入范围**不一致：A 把每条消息的 role/name/tool_call_id、每个 tool call 的 id/type、整个 tools 数组的 JSON、tool_choice 的 JSON 都按 rune/4 计入；B 只对"实际送给上游的那段 prompt 字符串"（工具提示已内联为文本）计数。故 A 的 prompt_tokens 系统性高于 B（结构字符、键名、逗号引号都成了 token）。
- 影响：跨实现用量不可比；A 的 prompt 估算偏保守（偏高），配合 A9-1 会直接放大成本估算。
- 修复：二选一并固化——要么按渲染后的 prompt 文本估算（B 路线），要么保留结构计数但同步修正 B 侧口径；至少去掉 `TrimSpace` 差异以保证同一文本同样结果。

### A9-11 [P2] 图片 usage 是造出来的数（64×n）与文本 prompt token；B 明确为 0 或缺省

- 本项目：`internal/grok/usage_estimate.go:160` — `func buildImageUsagePayload(prompt string, imageCount int) map[string]interface{} { promptTokens := approxTokenCount(prompt); completionTokens := max(0, imageCount) * 64; return map[string]interface{}{ "total_tokens": promptTokens + completionTokens, "input_tokens": promptTokens, "output_tokens": completionTokens, "input_tokens_details": map[string]interface{}{"text_tokens": promptTokens, "image_tokens": 0}, "prompt_tokens": promptTokens, "completion_tokens": completionTokens, ... } }`；调用点 `internal/grok/handler_images.go:301` — `"usage": buildImageUsagePayload(req.Prompt, len(data))`、`handler_images.go:75` — `"usage": buildImageUsagePayload(prompt, len(urls))`
- grok2api：`B:internal/infra/provider/web/image.go:1039` — `value["usage"] = map[string]any{ "total_tokens": 0, "input_tokens": 0, "output_tokens": 0, "input_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": 0} }`；非流式图片响应 `B:internal/infra/provider/web/image.go:1471` — `return jsonProviderResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data}), nil`（**完全没有 usage**）
- 差异/错误：图片生成不是 token 计费对象，B 流式事件里显式给 0、非流式直接不给 usage；A 却把 prompt 文本按 rune/4 折成 input token 并给每张图 64 个 output token（`64` 这个常数在本仓无任何来源）。附带一处口径 bug：A 把 `file` 输入块按图片计价——`usage_estimate.go:91` — `case "file": out.promptImageTokens += estimatedImagePromptTokens`（文件被当成 256 个 image token）。
- 影响：A 的图片请求会向客户端/审计报告不存在的 token 用量（再配合 A9-1 会变成不存在的成本）。
- 修复：图片路径与 B 对齐（流式报 0、非流式省略 usage），并删除 `file → image_tokens` 的错误归类。

### A9-12 [P2] A 的审计/运营聚合缺 cached/reasoning/total 与 priced/unpriced 维度

- 本项目：`internal/opsagg/opsagg.go:48` — `InputTokens int64` / `OutputTokens int64`（`Outcome` 只有这两个 token 字段）；`internal/api/api.go:1110` — `if tokens := event.InputTokens + event.OutputTokens; tokens > 0 { usage[event.AccountID] += int64(tokens) }`（per-account 观测值只累加 input+output）
- 本项目：`internal/audit/audit.go:48` — `CachedInputTokens int \`json:"cached_input_tokens,omitempty"\``（字段存在，`internal/grok/handler.go:198` 会写，但没有任何聚合/汇总读取它）
- grok2api：`B:internal/infra/persistence/relational/audit_repository.go:689` — `COALESCE(SUM(input_tokens), 0) AS input_tokens, COALESCE(SUM(cached_input_tokens), 0) AS cached_input_tokens, COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens, COALESCE(SUM(total_tokens), 0) AS total_tokens, COALESCE(SUM(estimated_cost_in_usd_ticks), 0) AS estimated_cost_in_usd_ticks, COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') <> '' THEN 1 ELSE 0 END), 0) AS priced_requests, ... COALESCE(SUM(CASE WHEN COALESCE(pricing_model, '') = '' THEN total_tokens ELSE 0 END), 0) AS unpriced_tokens`
- grok2api：`B:internal/application/audit/service.go:616` — `type SummaryUsage struct { ... InputTokens int64; CachedInputTokens int64; OutputTokens int64; ReasoningTokens int64; TotalTokens int64; ... PricedTokens int64; UnpricedTokens int64 }`；per-account 观测：`B:internal/infra/persistence/relational/audit_repository.go:579` — `Select("account_id, COALESCE(SUM(total_tokens), 0) AS total_tokens").Where("account_id IN ? AND created_at >= ? AND total_tokens > 0", ...)`
- 差异/错误：B 的汇总口径是"5 个 token 维度 + 成本 + 已计价/未计价 token"（`priced_tokens`/`unpriced_tokens` 直接服务于成本覆盖率），per-account 观测用 `SUM(total_tokens)`；A 只透传/累加 input+output，`CachedInputTokens`/`ReasoningTokens` 落库后无人汇总，也没有成本/覆盖率维度。
- 影响：无法回答"缓存省了多少""推理占了多少""多少用量没算钱"；A 的 free 额度估算与 B 的口径也因此只在 `total==input+output` 时偶然相等。
- 修复：A 的聚合读取与 `Summary` 输出补齐 cached/reasoning/total 及 priced/unpriced（配合 A9-1）。

### A9-13 [P2] 缺 B 新增的"按会话作用域 + call_id"推理证明缓存（19 commit 漂移）

- 本项目：`internal/grok/session_state.go:165` — `func replayMapKey(model, key string) string { return normalizeModelID(model) + "\x00" + strings.TrimSpace(key) }`（A 的 reasoning replay 只有"模型 + 会话键"一层，`session_state.go:234` 的 `storeReasoningReplayItems` 把整轮 items 存进这一个槽位，TTL 固定 `grokSessionStateTTL = time.Hour`，见 `session_state.go:20`）
- grok2api：`B:internal/infra/provider/conversation/reasoning_cache.go:53` — `func scopedReasoningCacheKey(scope, callID string) string { scope = strings.TrimSpace(scope); callID = normalizeReasoningCallID(callID); if scope == "" || callID == "" { return "" }; return scope + "\x00" + callID }`；`B:internal/infra/provider/conversation/reasoning_cache.go:11` — `defaultReasoningCacheCapacity = 4096` / `defaultReasoningCacheTTL = 30 * time.Minute`；`reasoning_cache.go:175` — `func (c *ReasoningCache) RememberReasoningForEnvelope(scope string, envelope responseEnvelope) { ... case "function_call": if current != nil && item.CallID != "" { c.SetScoped(scope, item.CallID, *current) } }`（`reasoning_cache.go:171` 注释：`A single reasoning item may legitimately precede several parallel calls; separate reasoning items are never all collapsed onto the last call.`）
- 差异/错误：B 在 HEAD 新增了 Build 平面"会话作用域 + call_id → reasoning 证明"的有界 LRU 缓存（含 `|` 后缀归一化与 `toolu_` 前缀等价候选，`reasoning_cache.go:64`/`:72`），用于把每个 function_call 与"紧邻其前"的 reasoning item 精确配对；A 只有整轮 replay 列表 + `filterReplayItemsForInput` 的"必须已有该 call 的 output 才回放"规则（`internal/grok/reasoning_replay_items.go:351` — `if len(keys) == 0 || anyReplayCallKeyExists(existingCalls, keys) { continue }`，`reasoning_replay_items.go:363` — `if outputCallID == "" { continue }`），没有按 call_id 的独立证明槽位、没有 4096/30min 的容量与 TTL 策略，也没有 `RememberReasoningForEnvelope` 的"多并行调用共享一个 reasoning item"关联。
- 影响：多轮 Build 工具循环中，B 能按 call_id 重新挂载证明（同一 reasoning item 可服务多个并行 call），A 只能整轮回放，且当客户端只回传部分历史时更容易整体丢弃 reasoning（`assistantMatches` 不匹配即 `return nil`）。这是 44a390b8→HEAD 之间新增能力的缺口，不是"改错"。
- 修复：移植 `conversation/reasoning_cache.go`（含 `RememberReasoningForEnvelope`、`reasoningCallIDCandidates`、LRU+TTL），并与现有 `session_state.go` 的 session 级 replay 并存。
