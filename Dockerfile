# 阶段1: 从 frp 官方镜像获取 frpc
FROM fatedier/frpc:v0.71.1 AS frpc

# 阶段2: 构建 Go 后端
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY main.go core.go .
RUN go mod init afrp-web && go build -o afrp-web .

# 阶段3: 最终运行镜像
FROM alpine:3.19
RUN apk add --no-cache ca-certificates

# 从官方镜像复制 frpc（保证版本正确，避免网络问题）
# 先复制到临时位置，然后查找并移动到正确位置
COPY --from=frpc / /tmp/frpc-image/
RUN FRPC_PATH=$(find /tmp/frpc-image -name "frpc" -type f 2>/dev/null | head -1) && \
    if [ -n "$FRPC_PATH" ]; then \
      cp "$FRPC_PATH" /usr/local/bin/frpc && chmod +x /usr/local/bin/frpc && rm -rf /tmp/frpc-image; \
    else \
      echo "frpc not found in image"; exit 1; \
    fi

# 记录内置核心版本：面板显示"恢复内置版本"的基线，也用于和清单最新版本对比
RUN mkdir -p /usr/local/share && \
    /usr/local/bin/frpc -v 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 > /usr/local/share/frpc-version && \
    echo "内置 frpc 版本：$(cat /usr/local/share/frpc-version)"

COPY --from=builder /build/afrp-web /usr/local/bin/afrp-web
COPY web/ /web/

EXPOSE 8000
CMD ["afrp-web"]
