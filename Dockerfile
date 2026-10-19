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
COPY --from=frpc /usr/local/bin/frpc /usr/local/bin/frpc

COPY --from=builder /build/afrp-web /usr/local/bin/afrp-web
COPY web/ /web/

EXPOSE 8000
CMD ["afrp-web"]
