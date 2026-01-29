# 部署指南

## 本地开发

### 安装依赖

```bash
go mod download
```

### 运行服务

```bash
go run ./cmd/server/main.go -config ./config.json
```

## 测试

### 运行测试

```bash
go test ./...
```

### 现有测试

- `internal/tiktoken/tokenizer_test.go`
  - Token 估算测试
  - 文本 Token 测试
  - CJK 字符检测测试
