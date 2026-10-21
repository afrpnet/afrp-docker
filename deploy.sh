#!/bin/bash
# afrp-web Docker 容器更新脚本
# 用法：ssh root@<服务器IP> 'bash -s' < deploy.sh

set -e

# 服务器地址，仅用于最后的提示信息；可由环境变量覆盖
SERVER_IP="${SERVER_IP:-<服务器IP>}"

echo "=== 更新 afrp-web 容器 ==="
echo "当前时间: $(date)"

# 停止并删除旧容器
echo "1. 停止旧容器..."
docker stop afrp-web 2>/dev/null || true
docker rm afrp-web 2>/dev/null || true

# 拉取最新镜像
echo "2. 拉取最新镜像..."
docker pull afrp/afrp:latest

# 启动新容器
echo "3. 启动新容器..."
docker run -d \
  --name afrp-web \
  --restart unless-stopped \
  -p 8000:8000 \
  -p 7400:7400 \
  -v afrp-data:/data \
  afrp/afrp:latest

echo "4. 检查容器状态..."
docker ps | grep afrp-web

echo "5. 确认配置卷已挂载..."
docker inspect afrp-web --format '{{json .Mounts}}'

echo ""
echo "=== 更新完成 ==="
echo "访问 http://${SERVER_IP}:8000/ 验证"
