# docs 索引

| 文档 | 内容 |
| --- | --- |
| `api-reference.md` | 对外 API：推理端点、管理端点、账号凭据格式 |
| `configuration.md` | 配置项逐项说明（含默认值与安全含义） |
| `architecture.md` | 包结构与请求链路 |
| `deployment.md` | 部署与运维（systemd、Caddy、nft、备份） |
| `grok2api-parity-audit.md` | Grok 通道 × chenyme/grok2api 的对账审计（171 条，全部已闭环） |
| `grok2api-fix-ledger.md` | 上述审计的修复台账，逐轮记录改动与部署校验 |
| `incident-2026-09-20-workbuddy-503.md` | 2026-09-20 两条 503 的定性与三轮修复 |

## 已归档（不在 HEAD，历史提交可取回）

2026-09-20 的文档清理把这些移出了 HEAD，**代码与运行状态不受影响**；需要时用 `git show` 取回。
基准提交 `546ee00`（该提交的 HEAD 仍含全部归档内容）：

```sh
# 9 份逐面对照证据档案
git show 546ee00:docs/grok2api-audit/06-accounts-quota.md

# 对账复算脚本
git show 546ee00:docs/grok2api-audit/recount.py > /tmp/recount.py

# 2026-09-19 线上诊断分析（长会话 token 分布、失败率）
git show 546ee00:docs/diag-analysis-2026-09-19.md

# 模型列表刷新审计与整改
git show 546ee00:docs/model-refresh-accuracy-audit.md
```

| 归档 | 归档时的体积 | 归档理由 |
| --- | --- | --- |
| `docs/grok2api-audit/01..09-*.md`、`recount.py` | 约 330 KiB | 逐行源码对照的过程稿；结论已收敛进 `grok2api-parity-audit.md` 与台账 |
| `docs/diag-analysis-2026-09-19.md` | 28 KiB | 一次性排障记录；支撑容量的实测数字已内联进事故文档 |
| `docs/model-refresh-accuracy-audit.md` | 20 KiB | 整改已完成并入账，且上下文窗口工作已重写 |

## 脱敏约定

本仓库是 **public**，文档不写生产凭据与可识别的部署标识：

- 口令、Key、token、Cookie、邮箱一律不出现；需要说明时写字段名或占位符。
- 生产/面板主机与域名写成占位符：`<PROD_IP>`、`<PROD_DOMAIN>`、`<PANEL_IP>`、`<PANEL_DOMAIN>`、
  `<PANEL_NAME>`、`<PROD_HOSTNAME>`、`<ALLOWED_SOURCE_IP_2>`、`<CLOUD_REGION>`、`<CLOUD_ASN>`、`<ACCOUNT_EMAIL>`。
- 本地专属路径（`.audit/`、`.upstream/`、`.gotmp/`）不写进文档：这些目录未纳入版本控制，读者拿不到。
  需要说明来源时写"（未纳入版本控制）"或直接内联结论。
