# 模型目录 / 路由对比

审计范围：模型 ID 集合、别名/前缀与大小写、模型分组、对外能力元数据、`/v1/models` 与 Codex/CLI 目录 JSON 形状、别名解析顺序与冲突、模型不存在/未授权错误、每模型下游路由选择与覆盖配置、每模型 reasoning effort 映射、默认 max output tokens、按模型拒绝不支持参数、上游目录同步。

代码基线：A = `/home/zhangdailin/Documents/Orchids-2api`；B = `A/.upstream/grok2api`（chenyme/grok2api HEAD 906b9493）。B 的路径一律以 `B:` 前缀、相对 `B/backend` 根给出。

## 结论摘要

- 公开模型 ID 的命名空间策略不同：A 把 Provider 前缀（`console/`、`build/`、`web/`）当成对外模型 ID 的一部分，B 把 Provider 前缀仅当内部路由 ID，对外一律剥离。`grok-4.3`、`grok-build-0.1`、`grok-imagine-video-1.5` 等名称在 A 里对下游客户端不可用或语义不同，而 B 直接暴露该名称，属于 P1 级互操作差异。
- A 完全缺失 reasoning-effort 后缀别名体系（`grok-4.5-low` 等）与注册式 Provider 兼容别名（`grok-4.5-console`、`grok-4.3-low` 等），B 两套都有，且第一套受 API Key 的 `allowModelAliases` 开关控制。A 的 API Key 结构中也不存在该开关。
- 同一个对外名称在 A 只能绑定一个 Provider（一行 `(channel, model_id)`），B 用 `PublicIDCandidateGroups` 把无前缀名称展开成多 Provider 候选，并按 Build→Web→Console 顺序回退。Build 无可用账号时 A 直接失败，B 会回落。
- Console 平面的“每模型”语义（`SupportsReasoning` / `SupportsReasoningEffort` / `DefaultReasoningEffort` / `MaxOutputTokens`）只在 B 存在。A 的 Console 归一化函数接收 `model` 参数却完全不用它：非推理模型照发 `reasoning`、固定推理模型照发 `effort`、缺省时不注入 `max_output_tokens`，而 B 分别做删除/删除/注入。
- Codex（`?client_version=`）目录形状不一致：A 少 5 个协议字段（`default_service_tier`、`availability_nux`、`upgrade`、`model_messages`、`auto_compact_token_limit`），工具能力判定放宽到“任意含 responses 能力的 Provider”（B 要求 `Provider==Build && Capability==Responses`），固定推理模型/未知模型的 `supports_reasoning_summary*` 结论也与 B 相反。
- 错误契约不一致：模型不存在时 A 返回 `400 text/plain`，B 返回 `404` JSON `model_not_found`。
- A 的 Build 账号目录不再补全 Composer / grok-4.5 / video 1.5（B 仍补全），因此同一账号在两边 `/v1/models` 上看到的集合不同。
- 工程/文档层：A 的前端兜底模型列表 15 个 ID 全部是 A 自己已弃用的 ID；`grok-imagine-image-pro` 在 A 的目录与图片端点白名单里仍然存在，但被自家弃用策略判死。
- `internal/grok/admin_meta.go` 只含 `verify`/`storage`/`voice-token` 三个端点，不含任何模型目录逻辑；`internal/grok/handler_switch_test.go` 是账号切换测试，与模型目录无关。这两个文件在本区域无发现。
- `internal/api/api.go` 的 `/api/models` 是管理端原始表接口（返回裸数组），公开模型列表实际由 `internal/handler/models.go` 提供；下文按真实实现引用。

## 发现

### A4-1 [P1] 对外模型 ID 携带 Provider 前缀（`console/…`、`build/…`），grok2api 对外剥离

- 本项目：`internal/grok/models.go:65` — `{ID: "console/grok-4.3", Name: "Console Grok 4.3", ConsoleModel: "grok-4.3", Tier: grokTierSuper, Upstream: UpstreamConsole},`
- 本项目：`internal/handler/models.go:143` — `entry := publicModelResponse(m.ModelID, mChannel)`（列表直接回显库里的 `ModelID`，即 `console/grok-4.3`）
- grok2api：`B:internal/infra/provider/console/catalog.go:25` — `{PublicID: "grok-4.3", UpstreamModel: "grok-4.3", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000},`
- grok2api：`B:internal/domain/model/model.go:143` — `func ExternalPublicID(provider account.Provider, value string) string { value = strings.TrimSpace(value); prefix := provider.ModelNamespace() + "/"; if provider.IsValid() && len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) { return strings.TrimSpace(value[len(prefix):]) }`
- grok2api：`B:internal/transport/http/inference/handler.go:255` — `publicID := modeldomain.ExternalPublicID(value.Provider, value.PublicID)`
- 差异/错误：B 内部路由 ID 是 `Console/grok-4.3`，对外名是 `grok-4.3`（`console/catalog.go:92` 用 `NormalizePublicID` 构造内部 ID，`ExternalPublicID` 还原对外名）。A 把 `console/` 写进了公开 `ModelSpec.ID`，并通过 store 行原样输出，因此 A 的 `/v1/models` 与 `/v1/models/{id}` 对下游暴露 `console/grok-4.3`、`console/grok-4.5`、`console/grok-imagine-image` 等名字。A 自己的测试也固化了这个方向：`internal/grok/handler_model_validation_test.go:172` — `for _, id := range []string{"grok-4.3", "grok-build-0.1", "grok-4.3-beta"} { if _, ok := ResolveModel(id); ok { t.Fatalf("ResolveModel(%s) = true, want removed", id) } }`，即裸 `grok-4.3` 被刻意判为不可解析。
- 影响：把 grok2api 的模型名当契约的客户端（配置里写 `grok-4.3`、`grok-4.5`、`grok-build-0.1`）在 A 上直接 400/404；反之把 A 当上游的客户端会拿到 B 不存在的带前缀名称。`grok-imagine-image`、`grok-chat-*` 这类不冲突的 ID 两边一致，冲突的 Console 系列全部错位。
- 修复：公开目录只输出 Provider 命名空间剥离后的名称（保留内部 ID 不变），或在解析入口同时接受裸名并映射到 `console/<id>`；补一条“裸名 == 唯一 Console 路由”的解析测试。

### A4-2 [P1] 缺少 reasoning-effort 后缀别名（`grok-4.5-low` 等）的合成与解析

- 本项目：`internal/handler/models.go:124` — `var publicModels []PublicModelResponse` / `:157` — `resp := PublicModelsListResponse{Object: "list", Data: publicModels}`（列表只回显库中行，无别名展开）
- 本项目：`internal/grok/handler.go:300` — `func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error { id := normalizeModelID(modelID); if IsDeprecatedModelID(id) { return fmt.Errorf("model not found") }`（后续仅按 `(grok, id)` 查库，无后缀解析）
- grok2api：`B:internal/transport/http/inference/handler.go:229` — `if allowAliases { items = appendReasoningModelAliases(items) }`
- grok2api：`B:internal/domain/model/reasoning.go:224` — `func reasoningAliasPublicIDs(publicModel string, levels []string) []string { base := strings.TrimSpace(publicModel); if base == "" { return nil }; if len(levels) < 2 { return nil }; external := externalModelSlug(base); ... aliases = append(aliases, external+"-"+level) }`
- grok2api：`B:internal/transport/http/inference/model_list_test.go:57` — `for _, want := range []string{"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.6", "grok-4.6-low", ..., "grok-4.3-none", "grok-4.3-low", "grok-4.3-medium", "grok-4.3-high", "grok-build-0.1"}`
- 差异/错误：B 会在列表里合成 `<model>-<effort>` 条目并在请求侧用 `ParseReasoningModelAlias` 反解（`B:internal/application/gateway/service.go:568` `if base, effort, ok := modeldomain.ParseReasoningModelAlias(publicModel); ok { ... }`），且只合成模型真正支持的档位（`grok-4.5-none`、`grok-4.5-xhigh` 被测试显式拒绝）。A 既不在列表里产出别名，也没有任何后缀解析函数（`grep ParseReasoningModelAlias` 在 A 无命中；A 只有面向 Warp `<family>-<effort>` 的 `internal/handler/handler_helpers.go:222 resolveEffortModelVariant`，它要求库里真存在后缀行，而 A 不会为 grok 建这种行）。A 的 API Key 结构体也没有对应开关（A 无 `AllowModelAliases` 字段，`internal/middleware/session.go:25` 只有 `AllowedModels []string`）。
- 影响：客户端把 `grok-4.5-high` / `grok-4.6-xhigh` 写死为模型名的调用在 A 上全部报模型不存在；A 的 `/v1/models` 无法让客户端发现可用的档位名。
- 修复：在 `ModelSpec`/`modelpolicy` 上实现 `SupportedReasoningEfforts` → `ReasoningAliasPublicIDs`，列表展开别名，并在 `ensureModelEnabled`/`resolveConversationModel` 入口先解析后缀别名再落到基础模型（同时按 Provider 剥离不支持档位）。

### A4-3 [P1] 缺少注册式 Provider 兼容别名（`grok-4.5-console`、`grok-4.3-low` …）

- 本项目：`internal/grok/models.go:109` — `func ResolveModel(modelID string) (ModelSpec, bool) { id := normalizeModelID(modelID); id = strings.TrimPrefix(id, "web/"); m, ok := modelByID[id]; return m, ok }`（只做一个 `web/` 前缀剥离，其余全靠精确 ID 命中）
- 本项目：`internal/grok/models.go:93` — `var modelByID = func() map[string]ModelSpec { ... out[strings.ToLower(strings.TrimSpace(m.ID))] = m ... }`（单一 map，无别名表）
- grok2api：`B:internal/infra/provider/console/catalog.go:59` — `consoleAlias("grok-imagine-image-quality-2.0", "grok-imagine-image-quality", "grok-imagine-image-quality", ""),` … `:66` — `consoleAlias("grok-4.3-low", "grok-4.3", "grok-4.3", "low"),` … `:64` — `consoleAlias("grok-4.5-console", "grok-4.5", "grok-4.5", ""),`
- grok2api：`B:internal/application/gateway/service.go:553` — `if alias, ok := s.providers.ResolveModelAlias(publicModel); ok { ... return []modeldomain.Route{route}, alias.ReasoningEffort, nil }`
- 差异/错误：B 有 14 条注册别名（`console/catalog.go:56-73`），其中既包括历史 PR 名称（`grok-imagine-image-quality-2.0`）、`-console` 后缀兼容名，也包括把 effort 固化到路由上的别名（`grok-4.3-low` 等，返回 `ReasoningEffort` 供上游改写）。A 无任何别名层，这些名称一律 400/404。A 的前缀剥离还是**白名单式**的：`normalizeModelID` 先小写，随后只 `TrimPrefix(id, "web/")`，所以 `Web/grok-chat-fast` 这类大小写变体能命中，但 `Build/grok-4.5`、`Console/grok-4.3` 前缀一律不剥（这些前缀本身又是 A 的公开 ID，见 A4-1）。
- 影响：任何依赖 `-console` 后缀或固化 effort 别名的客户端调用全部失败；从 grok2api 迁到 A 需要重写模型名。
- 修复：引入 provider 别名注册表（`Provider + Alias → PublicModel + ReasoningEffort`）并在 `ResolveModel` 之前查询；前缀剥离改为大小写不敏感的通用命名空间剥离。

### A4-4 [P1] 模型不存在/无权时的状态码与响应体不一致（400 text/plain vs 404 JSON）

- 本项目：`internal/grok/handler_chat.go:219` — `spec, ok := h.resolveConversationModel(r.Context(), req.Model); if !ok { http.Error(w, modelNotFoundMessage(req.Model), http.StatusBadRequest); return }`
- 本项目：`internal/grok/handler.go:467` — `func modelNotFoundMessage(modelID string) string { ... return fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", modelID) }`
- grok2api：`B:internal/transport/http/inference/handler.go:2309` — `case errors.Is(err, gateway.ErrModelNotFound): status, code = http.StatusNotFound, "model_not_found"; message = "模型不存在"`
- grok2api：`B:internal/application/gateway/service.go:40` — `ErrModelNotFound = errors.New("模型不存在或未启用")`
- 差异/错误：A 走 `http.Error`，因此 Content-Type 是 `text/plain`、状态码 400，且正文不是 OpenAI 错误对象；B 经 `writeOpenAIError` 输出 JSON `{"error":{"code":"model_not_found",...}}` 且状态码 404。A 的 `/grok/v1/messages` 同样有独立分支（`internal/grok/handler_messages.go:75`）但同样返回 400。探测/重试型客户端按“404 才是模型不存在”判断时，会把 A 的 400 当成请求格式错误而不重试或不去降级模型（A4-2/A4-9 的别名与回退路径尤其受影响）。
- 影响：错误分类、SDK 的 model-not-found 处理、监控告警分桶全部错位。
- 修复：统一改为 JSON 错误体 + 404（Anthropic 入口用 `not_found_error`），保留当前文案作为 `message`。

### A4-5 [P1] Console 平面缺“每模型默认 max_output_tokens”

- 本项目：`internal/grok/responses_normalize.go:151` — `if req.MaxTokens != nil && *req.MaxTokens > 0 { payload["max_output_tokens"] = *req.MaxTokens }`（客户端不给就不发）
- grok2api：`B:internal/infra/provider/console/catalog.go:25` — `... DefaultReasoningEffort: "medium", MaxOutputTokens: 1_000_000},`（`:26-30` 分别为 1_000_000 / 1_000_000 / 1_000_000 / 1_000_000 / 256_000）
- grok2api：`B:internal/infra/provider/console/normalize.go:42` — `if _, exists := payload["max_output_tokens"]; !exists && spec.MaxOutputTokens > 0 { payload["max_output_tokens"] = spec.MaxOutputTokens }`
- 差异/错误：B 的 Console 目录为每个模型声明默认输出上限并在缺省时注入；A 的 Console 路径没有任何等价的每模型默认值表（`internal/grok/models.go` 的 `ModelSpec` 也没有 `MaxOutputTokens` 字段），只在客户端显式传 `max_tokens` 时才发送。
- 影响：Console 路由（`console/grok-4.3`、`console/grok-4.5`、`console/grok-4.20-*`）在客户端不给上限时，上游按自身默认截断，长答案被提前切断且与 B 输出不一致。
- 修复：在 `ModelSpec` 增加 `MaxOutputTokens` 并在 Console 归一化里按模型注入默认值。

### A4-6 [P1] Console 平面缺“每模型 reasoning 剥离/默认档位”逻辑

- 本项目：`internal/grok/responses_normalize.go:718` — `func normalizeConsoleReasoningEffort(payload map[string]interface{}, model string) { reasoning, _ := payload["reasoning"].(map[string]interface{}); if reasoning == nil { return }; switch strings.ToLower(strings.TrimSpace(interfaceString(reasoning["effort"]))) { ... } }`（`model` 参数从未被使用）
- grok2api：`B:internal/infra/provider/console/normalize.go:192` — `func normalizeReasoning(payload map[string]any, spec ModelSpec) { if !spec.SupportsReasoning { delete(payload, "reasoning"); return } ... if !spec.SupportsReasoningEffort { delete(reasoning, "effort"); ... return } ... if effort == "" { effort = spec.DefaultReasoningEffort } ... }`
- grok2api：`B:internal/infra/provider/console/catalog.go:26` — `{PublicID: "grok-4.20-0309-reasoning", UpstreamModel: "grok-4.20-0309-reasoning", SupportsReasoning: true, MaxOutputTokens: 1_000_000},`（`SupportsReasoningEffort` 缺省=false → 走“删 effort”分支）
- 差异/错误：B 的 Console 目录用 `SupportsReasoning` / `SupportsReasoningEffort` / `DefaultReasoningEffort` 三个字段驱动三种行为：非推理模型整段删除 `reasoning`（`grok-4.20-0309-non-reasoning`、`grok-build-0.1`），固定推理模型只删 `effort`（`grok-4.20-0309-reasoning`），可配 effort 的模型在缺省时注入 `medium`（`grok-4.3`、`grok-4.5`）。A 的 `ModelSpec` 里没有这三个概念的等价物，`normalizeConsoleReasoningEffort` 只做别名改写，其余原样转发。
- 影响：A 把 `reasoning.effort` 发给明确拒绝该参数的 Console 模型（上游 400/参数被忽略），对 non-reasoning 模型发 `reasoning` 对象，且在客户端不提 effort 时不会得到 B 的 `medium` 默认，同一请求两边上游报文不同。
- 修复：在 Console 侧用 `modelpolicy` 的档位表驱动删除/注入，并把 `consoleFixedReasoningModels`（`internal/modelpolicy/reasoning.go:24`，A 已有但请求路径未使用）接到实际报文中。

### A4-7 [P1] Codex 目录缺少 5 个协议字段

- 本项目：`internal/handler/codex_models.go:35` — `type codexModelEntry struct { Slug string \`json:"slug"\` ... ServiceTiers []any \`json:"service_tiers"\`; BaseInstructions string \`json:"base_instructions"\`; ... EffectiveContextWindowPercent int \`json:"effective_context_window_percent"\`; ExperimentalSupportedTools []string \`json:"experimental_supported_tools"\`; InputModalities []string \`json:"input_modalities"\` }`（无 `default_service_tier`、`availability_nux`、`upgrade`、`model_messages`、`auto_compact_token_limit`）
- grok2api：`B:internal/transport/http/inference/codex_models.go:41` — `DefaultServiceTier *string \`json:"default_service_tier"\`` / `:42 AvailabilityNUX any \`json:"availability_nux"\`` / `:43 Upgrade any \`json:"upgrade"\`` / `:45 ModelMessages any \`json:"model_messages"\`` / `:59 AutoCompactTokenLimit *int \`json:"auto_compact_token_limit"\``
- 差异/错误：B 的这 5 个字段都没有 `omitempty`，即使为 nil 也会序列化成 `null`，因此 B 的 Codex 目录每个模型固定输出 5 个额外键；A 的结构体没有这些字段，JSON 里完全不存在。
- 影响：按字段存在性做解析/校验的 Codex/CLI 客户端在 A 的表单上会缺键（`model_messages`/`auto_compact_token_limit` 最可能被读取），并且 A 无法告知客户端 `default_service_tier`（服务档位）与自动压缩阈值。
- 修复：补齐 5 个字段并按 B 的零值语义输出（不加 `omitempty`）。

### A4-8 [P1] Codex 工具能力判定未限定 Build Provider

- 本项目：`internal/handler/codex_models.go:265` — `toolsSupported := codexHasCapability(item, "responses")`，配合 `:134` — `func codexHasCapability(item PublicModelResponse, capability string) bool { for _, value := range item.Capabilities { if strings.EqualFold(strings.TrimSpace(value), capability) { return true } } return false }`
- 本项目：`internal/store/store.go:742` — `default: model.Capabilities = []string{CapabilityChat, CapabilityMessages, CapabilityResponses}`（Web 系模型的默认能力集合里就含 `responses`）
- grok2api：`B:internal/transport/http/inference/codex_models.go:135` — `func codexAgentToolsSupported(item modelListItem) bool { return item.Provider == account.ProviderBuild && item.Capability == modeldomain.CapabilityResponses }`
- grok2api：`B:internal/infra/provider/web/catalog.go:19` — `{PublicID: "grok-chat-fast", UpstreamModel: "grok-chat-fast", Capability: modeldomain.CapabilityChat, Mode: "fast", MinimumTier: account.WebTierBasic},`
- 差异/错误：B 只在 Build + Responses 路由上打开 agent 工具集（`apply_patch_tool_type: "freeform"`、`supports_parallel_tool_calls: true`）；A 只要行的 capability 列表含 `responses` 就打开，而 A 为所有非媒体 Grok 行（含 Web/app-chat 行）默认写入 `chat,messages,responses`，Web 模型因此也被标成支持 agent 工具。
- 影响：Codex 客户端会对 A 的 Web 模型下发 `apply_patch`/并行工具调用等能力，而 Web app-chat 上游并不支持，表现为工具调用静默失败或 400。
- 修复：把判定改为 `provider == build && capabilities 含 responses`（A 的 `store.Model.Provider` 已有 `build`/`web`/`console` 三值）。

### A4-9 [P1] 同一对外名称无跨 Provider 回退（A 一名一 Provider，B 一名多路由 + 固定优先级）

- 本项目：`internal/grok/handler.go:423` — `switch strings.ToLower(strings.TrimSpace(model.Provider)) { case ProviderWeb: spec.Upstream = UpstreamAppChat ...; case ProviderConsole: spec.Upstream = UpstreamConsole ...; case ProviderBuild: spec.Upstream = UpstreamCLI ... }`（一个公开 ID 覆盖成恰好一个上游）
- 本项目：`internal/store/store.go:1123` — `func (s *Store) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error)`（按 `(channel, modelID)` 取唯一行，`channel` 固定为 `"grok"`，见 `internal/grok/handler.go:315`）
- grok2api：`B:internal/domain/model/model.go:174` — `group := make([]string, 0, len(account.Providers())); for _, providerValue := range account.Providers() { if normalized, ok := NormalizePublicID(providerValue, value); ok { group = append(group, normalized) } }`（无前缀名展开为全部 Provider 候选）
- grok2api：`B:internal/application/gateway/service.go:699` — `case accountdomain.ProviderBuild: return 0` / `:701` — `case accountdomain.ProviderWeb: return 1` / `:703` — `case accountdomain.ProviderConsole: return 2`（回退顺序）
- 差异/错误：B 对 `grok-4.5` 会同时得到 `Build/grok-4.5` 与 `Console/grok-4.5` 两条路由，`eligibleConversationRoutes` 过滤后在 `orderConversationRouteTargets` 里按 Build→Web→Console 排序，Build 不可用时自动降到 Console；A 把这两条路线做成两个不同的公开 ID（`grok-4.5` 与 `console/grok-4.5`），客户端点名 `grok-4.5` 时只会拿到 CLI spec，Build 账号池为空即整请求失败，不会落到 Console。
- 影响：单 Provider 容量/封禁/限流时 A 直接 5xx，B 自动切换；迁移客户端必须手工改模型名才能用上 Console 通道。
- 修复：公开名解耦 Provider（见 A4-1），在路由选择处保留多候选并按 Provider 优先级 + 账号可用性排序回退。

### A4-10 [P2] Codex `supports_reasoning_summary*` 对固定推理/未知模型的结论与 grok2api 相反

- 本项目：`internal/handler/codex_models.go:271` — `reasoningSupported := len(levels) > 0`，配合 `:109` — `func codexReasoningLevelsFor(publicID string) []string { return modelpolicy.SupportedReasoningEfforts(publicID) }`
- 本项目：`internal/modelpolicy/reasoning.go:51` — `func SupportedReasoningEfforts(publicID string) []string { slug := GrokModelSlug(publicID); if IsConsoleGrokModel(publicID) { if _, fixed := consoleFixedReasoningModels[slug]; fixed { return nil } } ... return []string{"none"} }`
- grok2api：`B:internal/transport/http/inference/codex_models.go:159` — `reasoningSupported := modeldomain.SupportsReasoningForProvider(item.Provider, item.ID)` / `:175` — `SupportsReasoningSummaryParameter: reasoningSupported, SupportsReasoningSummaries: reasoningSupported,`
- grok2api：`B:internal/domain/model/reasoning.go:139` — `func SupportsReasoningForProvider(providerValue account.Provider, publicModel string) bool { if IsFixedReasoningForProvider(providerValue, publicModel) { return true }; for _, effort := range SupportedReasoningEffortsForProvider(providerValue, publicModel) { if effort != ReasoningEffortNone { return true } }; return false }`
- 差异/错误：对 `console/grok-4.20-0309-reasoning`（固定推理、拒绝 effort），A 的档位表返回 nil → `reasoningSupported=false` → 目录宣告不支持 reasoning summary；B 的 `IsFixedReasoningForProvider` 命中（`B:internal/domain/model/reasoning.go:74`、B 测试 `B:.../model_list_test.go:174`）→ `true`。对未知模型，A 的 `SupportedReasoningEfforts` 兜底返回 `["none"]`（长度 1 → `true`），B 的 `SupportsReasoningForProvider` 对纯 `none` 返回 `false`。
- 影响：Codex 客户端对 A 的 Console 固定推理模型不下发 reasoning 摘要请求（丢失能力），对 A 的未知模型反而会下发摘要参数（B 会保守拒绝）。
- 修复：把判定换成“固定推理或存在非 none 档位”，与 `modelpolicy` 的固定推理表联动。

### A4-11 [P2] Build 账号目录补全（Composer / grok-4.5 / video 1.5）被 A 移除

- 本项目：`internal/grok/provider.go:225` — `// The snapshot records exactly what the upstream catalog returned. It used to be padded with a synthetic composer entry, a 4.5 alias whenever 4.6 was advertised, and a tier-gated video entry; those are locally invented capabilities ...`，`:234` — `changed := !slices.EqualFunc(acc.GrokModels, normalized, strings.EqualFold); acc.GrokModels = normalized`
- 本项目：`cmd/server/model_refresh.go:725` — `grok.ApplyCLIModels(result.account, result.models, now)`，`:731` — `for _, rawID := range result.account.GrokModels {`（发布集合直接来自未补全的快照）
- grok2api：`B:internal/infra/provider/cli/adapter.go:786` — `if credential.Provider == account.ProviderBuild && hasGrok46 { if _, exists := seen[buildGrok45Model]; !exists { seen[buildGrok45Model] = struct{}{}; result = append(result, buildGrok45Model) } }`
- grok2api：`B:internal/infra/provider/cli/adapter.go:795` — `if composer { if _, exists := seen[modeldomain.GrokComposer25Fast]; !exists { result = append(result, modeldomain.GrokComposer25Fast) } }`（另有 `:792` super 档补 `buildVideoModel`）
- 差异/错误：B 的 `NormalizeAccountModelCapabilities` 明确把三项“会话契约补全”写进账号能力快照（Composer、4.6 在位时的 4.5、super 的 video 1.5），并由同步写进路由目录；A 注释里承认删掉了这三种补全。于是同一 Build OAuth 账号在 B 的 `/v1/models` 里能看到 `grok-composer-2.5-fast`（以及 4.6 账号额外看到 `grok-4.5`），在 A 里只有当上游 `/models` 真的返回时才有（A 版本号/构建见 `internal/grok/cli.go:341 FetchModels`）。
- 影响：客户端按 B 的目录准备好 `grok-composer-2.5-fast` 模型名后，A 的列表里没有它（A 的 `ResolveModel` 仍能路由，但公开目录不可发现）；依赖目录做模型选择的客户端体验不一致。
- 修复：在 A 的 Build 快照归一化处恢复三条派生规则（或明确记录为已放弃的兼容差异并同步文档）。

### A4-12 [P2] `grok-imagine-image-pro` / `grok-imagine-image-quality` 在 A 仍是公开模型 ID，grok2api 已把 pro 改为 2.0 上的标志位并断言旧名不存在

- 本项目：`internal/modelpolicy/grok.go:22` — `"grok-imagine-image-pro": {},`（已弃用）
- 本项目：`internal/grok/models.go:78` — `{ID: "grok-imagine-image-pro", Name: "Grok Imagine Image Pro", UpstreamModel: "grok-imagine-image-pro", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},`
- 本项目：`internal/grok/handler_images.go:102` — `http.Error(w, "image generation model must be one of [grok-imagine-image-lite, grok-imagine-image, grok-imagine-image-2.0, grok-imagine-image-quality, grok-imagine-image-pro]", http.StatusBadRequest)`
- grok2api：`B:internal/infra/provider/web/catalog.go:29` — `{PublicID: "grok-imagine-image-2.0", UpstreamModel: "grok-imagine-image-2.0", ProtocolModel: "imagine", ImaginePro: true, Capability: modeldomain.CapabilityImage, Mode: "image_pro", MinimumTier: account.WebTierBasic},`
- grok2api：`B:internal/infra/provider/web/protocol_test.go:69` — `for _, removed := range []string{"grok-imagine-image-quality-lite", "grok-imagine-image-quality", "grok-imagine-image-quality-2.0", "grok-imagine-image-speed", "grok-imagine-image-pro"} { if _, exists := publicIDs[removed]; exists { t.Fatalf("obsolete image model remains: %s", removed) } }`
- 差异/错误：A 的 `SupportedModels` 里 `grok-imagine-image-pro` 与自家弃用表冲突——`ensureModelCapability` → `ensureModelEnabled` 第一行就 `IsDeprecatedModelID` 判死（`internal/grok/handler.go:302-304`），因此图片端点虽然把自己在 `handler_images.go:102/160` 的宣传语里列为支持，任何该模型的请求都会返回 `400 model not found`；同时 `imagineWSProModel`（`handler_images.go:206-210`）仍把它算作 pro 模型。B 已删除该公开 ID，把 pro 建模为 `grok-imagine-image-2.0` 的 `ImaginePro` 标志，并用测试锁死旧名不得回归。
- 影响：A 的模型目录/文档/前端会出现一个永远不可用的模型名；按目录探测能力的客户端会得到“列出但必失败”的条目（对照 A4-7 的 `visibility` 逻辑同样会受影响）。
- 修复：从 `SupportedModels`、`isKnownGrokMediaModelID`（`internal/grok/handler.go:442`）与图片端点白名单中删除 `grok-imagine-image-pro`，pro 能力改挂 `grok-imagine-image-2.0`。

### A4-13 [P2] 公开 `/v1/models` 条目字段集合与 `created` 语义不同

- 本项目：`internal/handler/models.go:18` — `type PublicModelResponse struct { ID string \`json:"id"\`; Object string \`json:"object"\`; Created int64 \`json:"created"\`; OwnedBy string \`json:"owned_by"\`; Capabilities []string \`json:"capabilities,omitempty"\`; Provider string \`json:"provider,omitempty"\`; UpstreamModel string \`json:"upstream_model,omitempty"\` }`
- 本项目：`internal/handler/models.go:33` — `func publicModelResponse(id, ownedBy string) PublicModelResponse { return PublicModelResponse{ID: id, Object: "model", Created: 1677610602, OwnedBy: ownedBy} }`（固定时间戳）
- grok2api：`B:internal/transport/http/inference/handler.go:194` — `type modelListItem struct { ID string \`json:"id"\`; Object string \`json:"object"\`; Created int64 \`json:"created"\`; OwnedBy string \`json:"owned_by"\`; Provider account.Provider \`json:"-"\`; Capability modeldomain.Capability \`json:"-"\` }`
- grok2api：`B:internal/transport/http/inference/handler.go:260` — `data = append(data, modelListItem{ID: publicID, Object: "model", Created: value.CreatedAt.Unix(), OwnedBy: "grok2api", ...})`
- 差异/错误：B 每条固定 4 个键，`created` 是路由行的真实创建时间，`provider`/`capability` 用 `json:"-"` 隐藏；A 多输出 `capabilities`/`provider`/`upstream_model`（会把内部 Provider 名与上游模型名泄露给下游），并把 `created` 固定为 `1677610602`。
- 影响：对严格校验 schema 的客户端是多余字段；`created` 全部相同会让按创建时间排序/增量的客户端逻辑失效（B 语义下每个模型不同）。
- 修复：若需保持兼容可保留扩展字段，但 `created` 应取行的创建时间，且考虑把 `provider`/`upstream_model` 移出公开响应。

### A4-14 [P2] `grok-imagine-image-lite` 在 A 排除 Basic 账号池，grok2api 的最低档位是 Basic

- 本项目：`internal/grok/models.go:120` — `case m.IsImage && normalizeModelID(m.ID) == "grok-imagine-image-lite" && m.Tier == grokTierBasic: return []string{"lite", "super", "heavy"}`
- 本项目：`internal/grok/handler_model_validation_test.go:407` — `next, err := h3.openChatAccountSessionForImagineLite(context.Background(), nil, spec); if err == nil { defer next.Close(); t.Fatalf("open image lite with only basic unexpectedly succeeded ...") }`
- grok2api：`B:internal/infra/provider/web/catalog.go:27` — `{PublicID: "grok-imagine-image-lite", UpstreamModel: "grok-imagine-image", ProtocolModel: "imagine-lite", Capability: modeldomain.CapabilityImage, Mode: "fast", MinimumTier: account.WebTierBasic},`
- grok2api：`B:internal/infra/provider/web/adapter.go:138` — `default: return []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}`
- 差异/错误：B 对 image-lite 的 `MinimumTier` 是 `basic`，`TierOrder` 默认分支把 Basic 排在第一位，因此 Basic（免费）Web 账号可以服务该模型；A 的 `PoolCandidates` 对同一公开 ID 显式返回 `lite/super/heavy`，把 `basic` 池排除，仅 Basic 账号的部署上该模型完全不可用（A 的测试固定了这个行为）。
- 影响：只接免费/基础 Web 账号的部署，B 能用 `grok-imagine-image-lite`，A 报“no available grok token”503。
- 修复：确认是否为有意的配额保护；若要与 grok2api 对齐，把 Basic 加回候选池（或改为按账号配额而非档位排除）。

### A4-15 [P3] 管理端模型列表/分组接口形状不同

- 本项目：`internal/api/api.go:2956` — `func (a *API) HandleModels(w http.ResponseWriter, r *http.Request) { ... case http.MethodGet: models, err := a.store.ListModels(r.Context()); ... json.NewEncoder(w).Encode(models) }`（裸数组，无分页/搜索/过滤）
- grok2api：`B:internal/transport/http/model/handler.go:116` — `response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})`
- grok2api：`B:internal/transport/http/model/handler.go:32` — `router.GET("/models", h.list); router.GET("/models/groups", h.listGroups); ... router.POST("/models/sync", h.sync)`，配合 `:415` — `return modelGroupResponse{Key: strings.Join(ids, ":"), Routes: routes, EndpointCapabilities: append([]string(nil), value.EndpointCapabilities...)}`
- 差异/错误：B 的管理端模型接口是分页信封（`items/page/pageSize/total`）+ 排序过滤 + 能力分组（含 `endpointCapabilities`）+ SSE 同步端点；A 的 `/api/models` 直接返回 `[]store.Model`，无分页元数据、无分组、无 `endpointCapabilities`，同步另走 `/api/models/refresh`（`cmd/server/routes.go:235`）。两者都是内部管理 API，不影响下游兼容，但同一前端无法直接复用。
- 影响：管理端迁移需重写前端；缺乏 `endpointCapabilities` 使“同名多能力路由”在管理界面上不可见。
- 修复：若要与 grok2api 管理端一致，改为信封响应并提供 groups/sync 端点。

### A4-16 [P3] 前端兜底模型列表与推理档位选项越界

- 本项目：`web/static/js/grok-tools.js:64` — `model: "grok-4.20-0309-non-reasoning", models: ["grok-4.20-0309-non-reasoning", "grok-4.20-0309", "grok-4.20-0309-reasoning", ... "grok-4.3-beta"]`（15 个 ID 与 `internal/modelpolicy/grok.go:5-55` 的弃用表逐条重合，无一在 `SupportedModels` 中）
- 本项目：`web/static/js/grok-tools.js:1841` — `for (const model of ["grok-4.20-0309-non-reasoning", "grok-4.20-fast", "grok-4.20-0309"]) { if (list.includes(model)) return model } return list[0] || "grok-4.20-0309-non-reasoning"`
- 本项目：`web/templates/pages/grok-tools.html:135` — `<select id="grokReasoningEffort" class="form-input"><option value="">自动</option><option value="none">关闭</option><option value="low">低</option><option value="medium">中</option><option value="high">高</option><option value="xhigh">极高</option></select>`（仅对 `grok-4.6` 禁用 `none`、对 console 4.20-reasoning 全禁，见 `web/static/js/grok-tools.js:1809-1817`）
- grok2api：`B:frontend/src/features/creative-console/creative-console-page.tsx:1760` — `function uniqueModelsByPublicID(models: ModelRouteDTO[]): ModelRouteDTO[] { ... }` 与 `:1771` — `return model?.provider === "grok_console" && model.upstreamModel === "grok-4.20-0309-reasoning"`（无硬编码模型清单，模型选项由 API 的 `modelOptions` 提供）
- 差异/错误：B 前端不内置 Grok 模型清单，档位/固定推理判定也按路由字段（`provider`/`upstreamModel`）计算；A 前端内置一份全部已弃用的兜底清单（接口失败时会被渲染成下拉项），并把 `none`/`xhigh` 暴露给所有模型（`grok-4.5` 不支持 `none`/`xhigh`，A 的 Build 归一化会把 `xhigh` 静默降为 `high`——`internal/grok/responses_normalize.go:700-711`）。
- 影响：抓取失败或未鉴权时管理界面提供一组必然被后端拒绝的模型名；档位下拉页让用户以为 `xhigh`/`none` 生效（实际被静默改写），与 B 的“目录驱动档位”不一致。
- 修复：删除硬编码兜底清单（或改用 `/grok/v1/models` 的 `capabilities` 与每模型档位元数据），档位选项按模型支持列表渲染。

### A4-17 [P3] Codex 未知模型默认描述文案不一致

- 本项目：`internal/handler/codex_models.go:102` — `var codexDefaultMetadata = codexModelMetadata{ contextWindow: 128000, description: "Grok model served via this gateway.", }`
- grok2api：`B:internal/transport/http/inference/codex_models.go:101` — `var grokDefaultCapability = grokModelCapability{ contextWindow: 128000, description: "Grok model served via grok2api.", }`
- 差异/错误：未知模型的 `context_window` 两边一致（128000，与 B 测试断言一致），只有 `description` 文案不同（"via this gateway" vs "via grok2api"）。上下文窗口/模态等实质元数据表（`codex_models.go:89-100` vs `B:.../codex_models.go:88-99`）逐条相同。
- 影响：仅文案，客户端不解析。
- 修复：无需处理，或统一为同一文案以降低 diff 噪声。
