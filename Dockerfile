# BinancePayTool 服务端镜像（二进制名沿用 bpaygate）
#   docker build -t bpaygate .
#   docker run -d -v $(pwd)/data:/data -p 8080:8080 bpaygate
# /data 内放 config.env（参考 config.example.env），数据库默认也写在 /data。
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bpaygate .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D bpg
USER bpg
WORKDIR /data
COPY --from=build /bpaygate /usr/local/bin/bpaygate
EXPOSE 8080
ENTRYPOINT ["bpaygate", "-config", "/data/config.env"]
