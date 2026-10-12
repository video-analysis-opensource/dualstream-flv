FROM registry.wolover.cn/zxiat-community/golang:1.25.9 AS builder
WORKDIR /src
COPY go_sample_code/go.mod go_sample_code/go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download
COPY ./go_sample_code .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /dualstream-flv .

FROM registry.wolover.cn/zxiat-public/ubuntu:24.04
ENV TZ Asia/Shanghai
ENV LANG C.UTF-8
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone
RUN apt-get update
RUN apt-get install -y vim htop lrzsz nload
COPY --from=builder /dualstream-flv /usr/local/bin/dualstream-flv
# 端口
EXPOSE 1397
EXPOSE 1398
ENTRYPOINT ["/usr/local/bin/dualstream-flv"]