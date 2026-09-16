# 模型管理「刷新模型列表」审计与整改记录

对象：`cmd/server/model_refresh.go`、`internal/qoder/`、`internal/store/store.go`、
`internal/modelpolicy/`、`internal/grok/provider.go`、`web/static/js/models.js`。

结论：**原实现的「刷新」在 Qoder 渠道上完全没有访问上游**，并且初始化会把 5 条渠道的
内置模型写进模型库；两者都已按「只依赖 active 账号 + 真实上游目录」整改，并已实测
Qoder 的有符号上游目录读取是**可用**的（此前代码里的「被上游拒绝」注释是错的）。

---

## 一、实测：Qoder 能否通过 COSY 签名获取上游列表

**能。** 用部署机 `us1.daige.tech` 上该部署自己的 active Qoder 账号（id=184）实测：

| 路由 | 方法 | 结果 |
|---|---|---|
| `/algo/api/v2/model/list` | GET | **HTTP 200，17 行** |
| `/algo/api/v2/model/list?FetchKeys=llm_model_result&Encode=1` | GET | **HTTP 200，17 行** |
| `/algo/api/v2/model/list` | POST | HTTP 400 `Request method 'POST' not supported` |

- 请求使用**聊天链路既有的 COSY 签名**（`Cosy-Key`/`Cosy-MachineId`/`Authorization: Bearer COSY.<payload>.<sig>`），未新增任何签名逻辑。
- 上游响应形状是**按能力分组**的对象：`{"chat":[{"key":..., "display_name":..., "max_input_tokens":..., "is_reasoning":...}]}`，不是裸数组。
- 因此「上游对 OAuth 凭据返回 `403 code=101 Signature invalid`」这条历史注释**不成立**（可能来自更早的、未签名或签名不同的调用）。

### 实测到的上游目录 vs 旧内置表

上游 17 行（`auto` 是路由指令，不计入）：

```
ultimate, performance, efficient, lite, sonus(smodel), cantus(cmodel),
qwen3.8-max, qwen3.8-flash, qwen3.7-max, qwen3.7-plus,
kimi-k3, kimi-k2.8-preview, glm-5.3, glm-5.3-flash,
deepseek-v4-pro, deepseek-flash, minimax-m3
```

与旧内置 18 条的差异（这就是内置表的实际危害）：

| 模型 | 旧内置表 | 上游实测 |
|---|---|---|
| `glm-5.2` / `gm51model` | 有 | **没有**（内置表凭空多出一个） |
| `kimi-k2.7-code` | 有 | 上游是 `kimi-k2.8-preview` |
| `deepseek-v4-flash` | 有 | 上游是 `deepseek-flash` |
| `sonus` / `smodel` | 没有 | **上游有**（内置表漏掉一个） |

即内置表同时**多报**和**漏报**了模型，仅靠刷新内置表永远对不齐上游。

---

## 二、整改内容

### 2.1 刷新只发布上游目录（`cmd/server/model_refresh.go`）

- 新增 `noActiveAccountsError`：渠道无 active 账号时**不拉取、不写入**，
  接口以 `{"skipped":true,"source":"no_active_account"}` 返回（HTTP 200），而不是伪造一次成功。
- 新增 `isUpstreamCatalogSource()` 闸门，`applyModelRefresh` 只接受白名单来源：
  `warp_graphql_*` / `grok_build_models` / `workbuddy_cli_models` /
  `qoder_upstream_models` / `puter_public_models_test_mode`。
  精确匹配（前缀匹配会把 `grok_build_models_unavailable_cached` 误判为上游）。
- **删除全部缓存回退**：`cachedGrokModels`、`cachedWarpModels` 及其分支已删；
  上游失败即报错，库中旧行只作「上次已知状态」保留，不再被重新发布成本次发现。
- **Qoder 不再发布内置表**：`qoder_builtin_catalog` 彻底移除，改为
  `fetchQoderUpstreamCatalogForRefresh` → `GET /algo/api/v2/model/list`（可注入，便于测试）。
- **Puter 不再发布未验证目录**：`puter_public_models_unverified` 分支删除；
  无 active 账号即跳过，逐模型探测通过的才发布（`source=puter_public_models_test_mode`）。
- **Warp 只用 enabled 账号**：`warpModelDiscoveryAccounts` 去掉 disabled 兜底。
- **`discovered` / `verified` 语义分离**：`discoveredModel` 增加 `Verified` 字段，
   由各渠道按真实观察结果设置，`applyModelRefresh` 不再写 `Verified = len(candidates)`。
- **剪枝条件收紧**：`shouldDeleteMissingModelsOnRefresh` 现在等价于
  `isUpstreamCatalogSource(source)`——只有在真正读到上游目录时才删除消失的模型。

### 2.2 初始化不再写入内置模型（`internal/store/store.go`）

- `seedModels()` → `prepareModels()`：只做「删除已废弃 ID」与「回填 Grok 路由元数据」，
  **不创建任何模型行**。全新部署的模型库是空的，完全由刷新从上游填充。
- 删除 `internal/store/{warp,grok,puter,workbuddy,qoder}_seed.go` 及
  `reconcileLatestPuterModels` / `reconcileLatestWorkBuddyModels` / `ensureRequiredGrokChatModels`
  （后者会在每次启动按内置表补行）。

### 2.3 移除请求链路里的内置白名单

- `internal/grok/provider.go` `ApplyCLIModels`：删除合成能力注入
  （无条件加 `grok-composer-2.5-fast`、有 4.6 就加 `grok-4.5`、按 Super 档位增删 video 1.5）。
  账号能力快照现在**等于上游目录**。
- `internal/modelpolicy/workbuddy.go`：删除 21 条内置白名单与 `LatestWorkBuddyModelIDs` / `IsLatestWorkBuddyModelID`。
- `internal/modelpolicy/puter.go`：删除 11 条内置白名单，改为 `PuterServiceForModel`
  按标识前缀推导上游 service（无法映射的仍拒绝）。
- `internal/modelpolicy/grok.go`：删除 `publicGrokModelIDs` 白名单，
  `IsVisibleGrokModel` 改为「非废弃 && 已被上游观察（verified）」。
- `internal/grok/handler.go`：无 store 时不再回退白名单，直接拒绝。
- `cmd/server/puter_public_models.go`：不再用内置表过滤上游目录，原样发布。

### 2.4 Qoder 目录解析（`internal/qoder/models_upstream.go`，新增）

- `FetchUpstreamModels`：复用聊天 COSY 签名，按序探测 GET 路由；
  失败返回错误（**无任何本地回退**）。
- `parseModelList` / `decodeCatalogEntries`：接受实测的 `{"chat":[...]}` 分组形状、
  裸数组、`data`/`models`/`list` 包装、`Encode=1` 的字符串嵌套，以及未列出的分组；
  只把**带 `key` 的行**当作模型，避免把无关对象数组误读成目录。
- `ProbeModelListRoutes`：诊断接口，逐路由报告状态，并在解析失败时带出原始响应片段
  （上游换形状时不至于只报「解析失败」）。
- `internal/qoder/catalog.go`：删除 `seedModels()` / `DefaultCatalog()` / `mergeCatalogs`；
  `Resolve()` 在无快照时返回 `ErrNoUpstreamCatalog`（不再回退内置表，也不再 panic）；
  `loadCatalog()` 返回空目录而非内置表。
- 快照格式升级为**每行一个 JSON 对象**（保留 `max_input_tokens` / `is_reasoning` / `is_vl` /
  `price_factor` / `format` / `source`），旧的 `"key\tname"` 形式仍可解析。
  否则聊天请求会丢掉 `context_length` 与推理标记。
- `internal/api/api_qoder{,_login}.go`：登录/校验时的目录读取改为有符号上游读取，
  失败则**留空快照**（不再安装内置表）。

### 2.5 前端（`web/static/js/models.js`）

- `modelRefreshSourceLabel` 覆盖全部真实来源，并对 `*_cached` / `*builtin*` / `*_unverified`
  显式标注「非上游目录，不应出现」。
- 新增 `skipped` 处理：无 active 账号时提示「未从上游拉取，也未写入模型」。
- 刷新摘要区分「无 active 账号」与「只读目录未逐个探测」两种 `同步 0` 的原因。

---

## 三、验证

在构建机（Go 1.26.6）上：

```
gofmt -l .        # 无输出
go vet ./...      # 通过
go test ./...     # 全绿（exit 0）
```

测试同步更新的要点：
- 删除/重写所有「依赖内置种子」的测试，改为显式发布模型行
  （`internal/handler` 的 `publishModel` 夹具等），断言的是「目录由上游发布」
  而不是「启动即有模型」。
- `internal/store/model_test.go` 新增 `TestStoreNew_PublishesNoBuiltInModels`
  （新库必须为空）与 `TestStoreNew_RemovesDeprecatedGrokModelsOnly`。
- `cmd/server/model_refresh_test.go` 新增来源闸门测试
  （`qoder_builtin_catalog` / `*_cached_models` / `*_unverified` / 空来源一律拒绝写入）
  与 `discovered`/`verified` 计数分离测试。
- `internal/qoder/models_upstream_test.go` 用**实测响应**作为夹具固定
  `{"chat":[...]}` 形状、编码嵌套、失败信封与路由必须为无 body 的 GET。
- `cmd/server/qoder_routes_test.go` 的端到端测试新增模型清单路由，
  断言刷新来源为 `qoder_upstream_models` 且带 COSY 签名头。

---

## 四、部署记录与部署中发现并修复的三个问题

### 4.1 部署

已部署到 `us1.daige.tech`，使用仓库自带的 `scripts/deploy-orchids.sh`
（checksum 校验 → 备份 → 重启 → 健康检查，失败自动回滚）：

```
version=manual-7a89f08
sha256=5f787c71619ba91a6c8e843f7933e92303e498eb4c329fbb729a05389528b32a
built_at=2026-09-16T20:37:33Z
```

部署后逐渠道实测（真实生产数据）：

| 渠道 | source | discovered | 结果 |
|---|---|---|---|
| Qoder | `qoder_upstream_models` | 17 | +3 −3，与上游完全对齐 |
| WorkBuddy | `workbuddy_cli_models` | 21 | +1（`deepseek-v4.1-flash-sg`） |
| Grok | `grok_build_models` | 2 | 文本目录，未剪枝 |
| Warp | `warp_graphql_feature_model_choice_all` | 93 | 与上游一致 |
| Puter | `puter_public_models_test_mode` | 51 | 上游全量目录 |

Qoder 对齐结果（实测）：删除内置表多出的 `glm-5.2`，把 `kimi-k2.7-code` 换成
上游的 `kimi-k2.8-preview`、`deepseek-v4-flash` 换成 `deepseek-flash`，
并新增内置表漏掉的 `sonus`。之后用 `model=sonus` / `model=qwen3.7-max`
实发聊天请求，均 HTTP 200 且模型正确解析。

### 4.2 部署中发现的三个问题（均已修复并再次部署）

**(1) Grok 被误剪枝（我引入的回归）**

`shouldDeleteMissingModelsOnRefresh` 一度对所有上游来源都返回 true，于是 Grok 也开始剪枝。
但 Build OAuth `/v1/models` 是**纯文本模型目录**，它不列举 Composer、图片、视频、语音与
STT 路由。结果一次刷新删掉了 **16 个仍在服务的模型**
（`grok-imagine-image`/`-video`/`-edit`/`-quality`/`-lite`、`console/grok-imagine-*`、
`grok-voice-*`、`grok-stt`、`grok-composer-2.5-fast`、`build/grok-imagine-video-1.5`），
`/grok/v1/images/*`、`/videos/*`、`/tts` 等端点随即 404。

修复：`grok_build_models` 明确排除在剪枝之外（恢复原有语义）。已用**部署前的完整快照**
逐行还原那 16 行（保留 provider / upstream_model / capabilities / verified / sort_order），
并加了回归测试。

**(2) 刷新从不把已有行标记为 verified**

原先只有「新建」才写 `Verified`；已存在的行永远保持 false。而我的可见性改动是
「非废弃 && verified」，于是渠道默认模型 `grok-4.6`（行存在但 verified=false）
从 `/v1/models` 消失并返回 404 —— 说明旧代码的 `publicGrokModelIDs` 白名单一直在**掩盖**
这个数据问题。

修复：`applyModelRefresh` 在观察到某行时把该行提升为 `verified`（只改这一个标志，
name/status/排序/默认值仍归运营所有）。实测刷新返回 `updated: 1`，
`/grok/v1/models/grok-4.6` 恢复 200。

**(3) 废弃 ID 清理不分渠道**

`cleanupDeprecatedModelIDs` 只按 `model_id` 匹配，于是每次启动都会删掉
**任何渠道**上同名（但合法）的行：Puter 上游确实通过 x-ai 提供 `grok-4.3`、
`grok-4.20-0309-*`，Warp 上游确实提供 `grok-build-0.1`。结果是「刷新发布 → 重启删除」
反复抖动。

修复：废弃表改为按渠道分组（`deprecatedModelIDsByChannel`），只从「退回该 ID 的渠道」
删除。实测：重启后 200 个模型**零丢失**（修复前同一操作会丢掉 4 个）。

### 4.3 需要运营决定的两件事

- **Puter 现在发布上游全量目录（51 条）**，包含 `gpt-4o`、`gpt-4.1`、`gemini-2.5-pro`
  等上一代模型。旧实现的 11 条内置白名单会把这些过滤掉。这是「不内置、以上游为准」
  的直接结果，且每一条都经过 `test_mode` 探测通过；如果不想暴露旧模型，
  需要的是**过滤策略**而不是内置目录 —— 请告知是否要加回。
- **`ErrNoUpstreamCatalog` 状态**：某渠道若从未成功读到上游目录，其模型列表为空是
  **预期**的，运营动作就是「用 active 账号刷新一次」。

### 4.4 部署环境风险

`us1.daige.tech` 根分区只有 6.8G，多次构建把它推到 97–99%（在该水位 Redis 写入开始失败）。
部署过程中我清理了自己的构建缓存与 `/tmp` 产物，每次均恢复到 86%。
建议：扩容数据盘，或把构建放到 CI/其他机器（release.yml 已经产出 artifact，
`scripts/deploy-orchids.sh --artifact` 可直接安装），不要在跑服务的机器上编译。

## 五、遗留说明

- `internal/qoder` 保留 `ErrNoUpstreamCatalog` 这条「未观察」状态。
- 账号快照已升级为 JSON 行（含 `max_input_tokens`/`is_reasoning` 等），
  旧 `"key\tname"` 快照仍可解析。
- `prepareModels()` 不再写入任何内置模型：全新部署（如清空 Redis）后，
  模型列表为空，需要每个渠道各刷新一次。这是符合要求的预期行为。
