# Build the manager binary
FROM release-ci.daocloud.io/demo/golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# 设置golang下载的国内代理
RUN go env -w GOPROXY='https://goproxy.cn,direct'
RUN go mod download

# Copy the go source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/
COPY pkg/ pkg/


RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go

FROM release-ci.daocloud.io/baize/snapshot-controller:v0.2.8 AS snapshot-controller

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM release-ci.daocloud.io/demo/ubuntu:22.04
WORKDIR /

#ENV DOCKER_VERSION=26.1.3
#ENV NERDCTL_VERSION=2.1.2
# mabing: 把镜像源替换为阿里的
#RUN sed -i 's|http://.*archive.ubuntu.com|http://mirrors.aliyun.com|g; s|http://.*security.ubuntu.com|http://mirrors.aliyun.com|g' /etc/apt/sources.list

# mabing: 直接从已有的镜像里直接拷贝了
#RUN apt-get update -y && apt-get install -y curl && apt-get clean && \
#    curl -fsSL https://download.docker.com/linux/static/stable/$(uname -m)/docker-${DOCKER_VERSION}.tgz | tar -xzC /usr/local/bin --strip-components=1 docker/docker && \
#    curl -fsSL https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-$(uname -m | sed 's/x86_64/amd64/g;s/aarch64/arm64/g').tar.gz \
#    | tar -zxC /usr/local/bin nerdctl
COPY --from=snapshot-controller /usr/local/bin/nerdctl /usr/local/bin/nerdctl
COPY --from=snapshot-controller /usr/local/bin/docker /usr/local/bin/docker

COPY --from=builder /workspace/manager .

ENTRYPOINT ["/manager"]
