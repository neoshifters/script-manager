# 阶段三：单文件 Go 服务 + 浏览器端，镜像固定自带 ffmpeg / ffprobe
# 构建: docker build -t script-manager:2.0 .
# 运行: docker run -d --name script-manager --restart always -p 8686:8686 -v /你的路径/脚本管理:/data script-manager:2.0

FROM golang:1.23-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod ./
# 通配复制所有 .go：以后新增文件不会因这里漏写而构建失败；_test.go 会被 go build 忽略
COPY *.go ./
COPY web ./web
RUN go build -trimpath -ldflags="-s -w" -o /out/script-manager .

FROM alpine:3.20
# ffmpeg / ffprobe 固定装进镜像（/usr/bin/ffmpeg、/usr/bin/ffprobe），不再依赖构建参数。
# 构建时顺手打印一次版本：如果 apk 没装上，这里会直接让构建失败，而不是等到界面上才发现找不到。
RUN apk add --no-cache ca-certificates tzdata ffmpeg \
    && ffmpeg -version | head -n 1 \
    && ffprobe -version | head -n 1
WORKDIR /app
COPY --from=build /out/script-manager /app/script-manager
ENV SM_ADDR=:8686 \
    SM_DATA=/data \
    TZ=Asia/Shanghai
EXPOSE 8686
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8686/api/health || exit 1
ENTRYPOINT ["/app/script-manager"]
