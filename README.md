# AFRP Web UI

Docker 版 AFRP 内网穿透客户端，通过浏览器管理 frpc。

## 快速开始

```bash
docker run -d -p 8000:8000 -v afrp-data:/data afrp/afrp
```

访问 http://localhost:8000

## 功能

- 登录认证（OIDC）
- 服务器选择
- 隧道管理
- 启动/停止 frpc
- 实时日志
- 配置持久化（Docker volume）

## 开发

```bash
go run main.go
# 访问 http://localhost:8080
```

## 构建

```bash
docker build -t afrp/afrp .
```
