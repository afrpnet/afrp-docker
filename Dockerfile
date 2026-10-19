# 阶段1: 从 frp 官方镜像获取 frpc
FROM fatedier/frpc:v0.71.1 AS frpc

# 阶段2: 构建 Go 后端
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY main.go .
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

COPY --from=builder /build/afrp-web /usr/local/bin/afrp-web
COPY web/ /web/

EXPOSE 8000
CMD ["afrp-web"]
