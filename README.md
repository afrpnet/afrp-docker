# AFRP Web UI

<div align="center">
  <img src="https://raw.githubusercontent.com/afrpnet/afrp-docker/master/logo-banner.png" alt="AFRP Logo" width="320"/>
</div>

Docker 版 AFRP 内网穿透客户端，通过浏览器管理 frpc。无需安装桌面应用，一键部署即可使用。

## 特性

- 🌐 **纯 Web 界面**：浏览器访问，跨平台兼容，无需安装客户端
- 🔐 **OIDC 认证**：复用官网账号登录，密钥按账号独立保存
- 👥 **多账号切换**：登录页可保存多个账号，一键切换，各自记住服务器与密钥
- 🖥 **服务器记忆**：每个账号记住上次选择的服务器，登录后自动回填，无需重新选择
- 🔒 **连接中锁定**：frpc 运行期间服务器下拉框锁定，避免误切节点导致断连
- 🛠 **隧道管理**：Web 界面增删改隧道（TCP/UDP/HTTP/HTTPS/STCP/SUDP/XTCP）
- 📊 **实时日志**：Web 界面查看 frpc 运行日志
- 💾 **配置持久化**：Docker volume 保存配置、密钥、隧道
- ✅ **官方内核**：直接使用 fatedier/frp 官方镜像中的 frpc，保证版本正确性

## 快速开始

```bash
# 拉取镜像
docker pull afrp/afrp

# 运行容器（配置持久化到 docker volume）
docker run -d \
  --name afrp-web \
  --restart unless-stopped \
  -p 8000:8000 \
  -v afrp-data:/data \
  afrp/afrp

# 访问 Web UI
# http://localhost:8000
```

> ⚠️ `-v afrp-data:/data` 必须挂载。配置、Client Secret、隧道都存放在 `/data` 下，漏挂卷会新建匿名卷，表现为「重启后又要重新登录、密钥又变成未设置」。

## 使用说明

1. **登录**：输入官网账号和密码，勾选「记住我」可保存账号以便下次一键登录
2. **服务器**：选择节点。系统会按账号记住这次选择，下次登录自动回填；frpc 运行期间下拉框锁定，需先停止才能切换。标题右侧会显示当前节点的分组徽章（免费 / VIP / SVIP / Pro）
3. **Client Secret**：首次使用需粘贴官网「连接说明」里的密钥，或在页面点击「生成新密钥」由官网接口签发。密钥按账号保存，切换账号时自动取用对应密钥并写入 `frpc.toml`
4. **隧道**：点击「+ 添加隧道」填写名称、类型、本地 IP/端口等信息
5. **启动**：点击「启动」按钮，frpc 开始运行；「停止」可断开
6. **监控**：查看运行状态与实时日志
7. **主题**：右上角按钮切换深色 / 浅色，默认跟随系统，选择结果记在浏览器本地

## 升级

镜像发布后按以下顺序更新（**务必保留数据卷**）：

```bash
docker stop afrp-web && docker rm afrp-web
docker pull afrp/afrp:latest
docker run -d --name afrp-web --restart unless-stopped -p 8000:8000 -v afrp-data:/data afrp/afrp:latest
```

仓库内的 `deploy.sh` 已包含上述命令，可直接执行：

```bash
ssh root@<服务器地址> 'bash -s' < deploy.sh
```

## 配置说明

### 数据持久化

配置和日志保存在 Docker volume `afrp-data` 中：

```bash
# 查看配置目录
docker exec afrp-web ls /data

# 备份配置
docker run --rm -v afrp-data:/data -v $(pwd):/backup alpine tar czf /backup/afrp-backup.tar.gz /data

# 恢复配置
docker run --rm -v afrp-data:/data -v $(pwd):/backup alpine tar xzf /backup/afrp-backup.tar.gz -C /
```

| 路径 | 说明 |
|------|------|
| `/data/config/config.json` | 主配置：账号、服务器、密钥、隧道列表 |
| `/data/config/frpc.toml` | 自动生成的 frpc 配置，启动时直接使用 |
| `/data/logs/frpc.log` | frpc 运行日志 |

`config.json` 中几个关键字段：

| 字段 | 说明 |
|------|------|
| `user` | 当前登录的官网用户名（frpc 的 `clientID` 即此值） |
| `account_secrets` | 账号 → Client Secret 映射，切换账号时按账号取用 |
| `account_servers` | 账号 → 最后选择的服务器 ID，登录时自动恢复 |
| `client_secret` | 当前账号的密钥（旧版单账号配置，升级后会自动归档进 `account_secrets`） |

### frpc 版本

frpc 二进制直接从 `fatedier/frpc:v0.71.1` 官方镜像复制，无需手动下载，保证版本正确性和兼容性。如需升级版本，修改 `Dockerfile` 中的 `FROM fatedier/frpc:vX.Y.Z` 后重新构建：

```bash
docker build -t afrp/afrp .
```

## API 接口

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/login` | POST | 官网账号密码登录，返回服务器列表、账号上次选择的服务器等 |
| `/api/user` | GET | 当前登录用户信息（含 OIDC 认证状态、密钥是否已设置） |
| `/api/config` | GET | 获取当前配置 |
| `/api/config` | POST | 保存配置（服务器地址/端口/ID、Client Secret） |
| `/api/secret` | GET | 获取当前账号的 Client Secret |
| `/api/secret` | POST | 保存或轮换 Client Secret（`action: save` / `rotate`） |
| `/api/tunnels` | GET | 隧道列表 |
| `/api/tunnels` | POST | 新增或更新隧道（按名称覆盖） |
| `/api/tunnels` | DELETE | 删除隧道（按名称） |
| `/api/start` | POST | 启动 frpc |
| `/api/stop` | POST | 停止 frpc |
| `/api/status` | GET | 运行状态，同时返回当前账号选中的服务器 ID |
| `/api/log` | GET | 获取最新日志 |

## 开发

```bash
# 本地运行 Go 后端（需 Go 1.22+）
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
├── deploy.sh            # 服务器端容器更新脚本
└── .github/workflows/   # CI 自动构建推送（打 tag 触发）
```

## 许可证

MIT
