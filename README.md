# Orchids-2api 文档

## 项目简介

**Orchids-2api** (orchids-api) 是一个 Go 语言编写的 API 代理服务器，提供多账号管理与负载均衡代理功能，兼容 Claude API 格式的请求转发。

### 核心功能

- 多账号管理与负载均衡代理
- 兼容 Claude API 格式的请求转发
- 将请求代理到 Orchids 后端服务
- 提供 Web 管理界面


## 文档目录

| 文档 | 描述 |
|------|------|
| [架构设计](./docs/architecture.md) | 目录结构、核心组件、请求流程、数据模型 |
| [API 接口](./docs/api-reference.md) | 所有端点列表、请求/响应格式、认证说明 |
| [部署指南](./docs/deployment.md) | 本地开发、生产部署 |
| [配置说明](./docs/configuration.md) | 配置文件格式 |

## 快速开始

```bash
# 本地开发
go mod download
go run ./cmd/server/main.go -config ./config.json
```

## 主要特性

1. **多账号管理** - 支持添加、编辑、删除多个 Orchids 账号
2. **负载均衡** - 加权随机算法分配请求
3. **故障转移** - 账号失败时自动切换
4. **模型映射** - 透明映射 Claude 模型到上游模型
5. **工具调用** - 完整支持 Claude Tool Use
6. **流式响应** - SSE 实时响应
7. **Token 计数** - 估算输入/输出 Token
8. **调试日志** - 详细的请求/响应日志
9. **管理界面** - Web UI 管理账号
10. **导入导出** - 账号配置备份恢复
