# 构建阶段
FROM golang:1.22-alpine AS builder
WORKDIR /build
COPY main.go .
RUN go mod init afrp-web && go build -o afrp-web .

# 运行阶段
FROM alpine:3.19
RUN apk add --no-cache ca-certificates

# 下载 frpc
ARG FRPC_VERSION=0.71.0
RUN wget -q https://github.com/fatedier/frp/releases/download/v${FRPC_VERSION}/frp_${FRPC_VERSION}_linux_amd64.tar.gz \
    && tar -xzf frp_${FRPC_VERSION}_linux_amd64.tar.gz \
    && cp frp_${FRPC_VERSION}_linux_amd64/frpc /usr/local/bin/frpc \
    && chmod +x /usr/local/bin/frpc \
    && rm -rf frp_${FRPC_VERSION}_linux_amd64*

COPY --from=builder /build/afrp-web /usr/local/bin/afrp-web
COPY web/ /web/

EXPOSE 8080
CMD ["afrp-web"]
