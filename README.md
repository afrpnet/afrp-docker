# AFRP Web UI

<div align="center">
  <img src="https://raw.githubusercontent.com/afrpnet/afrp-client/main/assets/brand/app_icon_source.png" alt="AFRP Logo" width="128"/>
</div>

Docker 版 AFRP 内网穿透客户端，通过浏览器管理 frpc。无需安装桌面应用，一键部署即可使用。

## 特性

- 🌐 **纯 Web 界面**：浏览器访问，跨平台兼容
- 🔐 **OIDC 认证**：复用官网账号登录
- ⚡ **一键启动**：Docker 单命令部署
- 💾 **配置持久化**：Docker volume 保存配置
- 📊 **实时日志**：Web 界面查看 frpc 运行日志
- 🔄 **自动更新**：CI 自动构建最新镜像
- ✅ **官方内核**：直接使用 fatedier/frp 官方镜像中的 frpc，保证版本正确性

## 快速开始

```bash
# 拉取镜像
docker pull afrp/afrp

# 运行容器（配置持久化到 docker volume）
docker run -d \
  --name afrp \
  -p 8000:8000 \
  -v afrp-data:/data \
  afrp/afrp

# 访问 Web UI
# http://localhost:8000
```

## 使用说明

1. **登录**：输入官网账号和密码，选择服务器
2. **配置**：填写 OIDC 认证参数（自动填充默认值）
3. **启动**：点击"启动"按钮，frpc 开始运行
4. **监控**：查看状态和实时日志

## 配置说明

### 数据持久化

配置和日志保存在 Docker volume `afrp-data` 中：

```bash
# 查看配置目录
docker exec afrp ls /data

# 备份配置
docker run --rm -v afrp-data:/data -v $(pwd):/backup alpine tar czf /backup/afrp-backup.tar.gz /data

# 恢复配置
docker run --rm -v afrp-data:/data -v $(pwd):/backup alpine tar xzf /backup/afrp-backup.tar.gz -C /
```

### 环境变量（可选）

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `FRPC_VERSION` | frpc 版本（构建时指定） | `0.71.0` |

**注意**：frpc 二进制直接从 `fatedier/frp:v0.71.0` 官方镜像复制，无需手动下载，保证版本正确性和兼容性。

```bash
# 如需使用其他版本，修改 Dockerfile 中的 FROM fatedier/frp:vX.Y.Z
docker build -t afrp/afrp --build-arg FRPC_VERSION=0.70.0 .
```

## API 接口

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/config` | GET | 获取当前配置 |
| `/api/config` | POST | 保存配置 |
| `/api/start` | POST | 启动 frpc |
| `/api/stop` | POST | 停止 frpc |
| `/api/status` | GET | 查询运行状态 |
| `/api/log` | GET | 获取最新日志 |

## 开发

```bash
# 本地运行 Go 后端
go run main.go

# 访问 http://localhost:8000
```

## 构建

```bash
# 本地构建镜像
docker build -t afrp/afrp .

# 运行测试
docker run -p 8000:8000 afrp/afrp
```

## 项目结构

```
afrp-web/
├── main.go              # Go 后端（HTTP API + frpc 进程管理）
├── web/index.html       # Web UI（原生 HTML/JS）
├── Dockerfile           # 多阶段构建（从 fatedier/frp 官方镜像复制 frpc）
└── .github/workflows/   # CI 自动构建推送
```

## 许可证

MIT
