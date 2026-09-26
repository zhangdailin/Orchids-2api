# 未使用文件排查与清理

## 范围与方法

本轮检查 Git 管理的 Go 后端、Web 静态资源与模板、测试、CI、生成脚本、部署文件和文档。Go 包依赖从 `cmd/server`、`cmd/providerregistry` 入口检查；文件级候选再核对符号引用、初始化、嵌入资源和测试用途。Web 文件核对模板链接、动态页面选择、路由及嵌入机制。未引用并不自动等于可删除。

39 个 Go 包均位于入口依赖闭包内。对 cmd/internal 的 207 个生产 Go 文件进行了文件级候选筛查；未发现可整体删除的无用包。筛查不是所有函数均有效的形式化证明。

## 删除清单

| 文件 | 删除依据 |
| --- | --- |
| `scan_exports.js` | 无项目、CI 或文档调用；仅扫描 JS/TS export，硬编码本地目录，不适用于本项目 Go 与普通全局 JS 函数的使用分析。 |
| `find_unused_exports.js` | 同类过时扫描脚本，亦无调用；与另一个脚本重复但不完全相同，不再保留任意一份作为可靠分析工具。 |
| `unused_exports.json` | 上述脚本产物，仅包含 `[]`，无消费者。 |
| `internal/util/jwt.go` | 唯一函数 JWTEmail 无生产调用，只有专用单元测试引用。 |
| `internal/util/jwt_test.go` | 仅测试已退役的 JWTEmail，不测试其他生产逻辑。 |
| `internal/handler/system_sanitize.go` | 唯一函数 sanitizeSystemItems 原样返回输入，旧兼容入口已无生产调用。 |
| `internal/handler/fidelity_test.go` | 仅测试已无生产调用的 sanitizer；真实 HandleMessages 透传回归仍由 handler_integration_test.go 和 handler_conversation_test.go 覆盖。 |

## 关联修复

- `.github/workflows/release.yml` 原来引用已不存在的 `web/grok_tools.test.cjs`。改为 `node --test web/*.test.cjs`。
- `.github/workflows/ci.yml` 同步使用该通配入口，运行全部前端测试，避免硬编码列表遗漏现存测试。
- 上一轮 `web/static/js/common.js` 的无用全局 formatBytes 删除保持不变。

## 明确保留

- 所有现有页面 JS/CSS、登录页、页面模板、侧边栏和模态框模板均有加载或包含路径。
- `provider-registry.js` 是生成文件，但被页面使用，且有生成器和一致性检查，保留。
- Go embed 的压缩提示词、Web 资源及测试夹具保留。
- `internal/workbuddy/live/*_test.go` 是受构建标签控制的现场测试，不因默认测试不加载而删除。
- 文档、部署脚本、服务配置不因缺少代码调用而删除。
- 不触碰已有未跟踪 `.tools/`、`function_analysis_report.md`；后者可能保留已删除符号的历史记录，不是代码引用。
- 不清空忽略目录 `dist/`、`.rsh/`、运行时数据及凭据；本轮不是磁盘缓存/运行数据清理。

## 验证方式

- 删除前运行全量 `go test ./... -count=1 -p 1` 作为基线。
- 删除后再次运行全量 Go 测试和 `go build ./...`。
- 运行 `node --test web/*.test.cjs`（107 项）。
- 运行 `./scripts/check-provider-registry.sh` 和 `git diff --check`。
- 结果：删除前后全量 Go 测试均通过；删除后 Go 构建通过；前端 107 项全部通过；注册表一致性与 diff 检查通过。
- 删除后首次 Go 验证因默认编译缓存只读而未完成；经授权重跑后全部通过，并非代码测试失败。
- 未进行真实上游登录、live-tag 现场测试或部署操作。
