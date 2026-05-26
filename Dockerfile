# Copyright 2024-2026 the original author or authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Multi-stage Dockerfile for KOM MCP Server
# - Builder: compiles the Go binary with the Go version required by go.mod.
# - Runtime: runs as a non-root user with explicit app and kubeconfig directories.

ARG GO_VERSION=1.24
ARG ALPINE_VERSION=3.20

FROM golang:${GO_VERSION}-alpine AS builder

ARG TARGETARCH=arm64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/kom-mcp ./main.go

# 下载与集群兼容的 kubectl 二进制（自动识别 amd64 或 arm64）
# Download the stable kubectl binary for the correct architecture
RUN KUBECTL_VERSION=$(wget -qO- https://dl.k8s.io/release/stable.txt) && \
    wget -qO /out/kubectl \
      "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" && \
    chmod +x /out/kubectl

FROM alpine:${ALPINE_VERSION}

ARG PORT=9096
ARG APP_HOME=/app
ARG APP_USER=appuser
ARG APP_GROUP=appgroup
ARG APP_UID=10001
ARG APP_GID=10001

LABEL maintainer="AgentScope Team" \
      description="KOM MCP Server" \
      org.opencontainers.image.title="kom-mcp" \
      org.opencontainers.image.description="Kubernetes multi-cluster MCP server powered by KOM" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g "${APP_GID}" -S "${APP_GROUP}" && \
    adduser -u "${APP_UID}" -S -D -h "${APP_HOME}" -G "${APP_GROUP}" "${APP_USER}" && \
    mkdir -p "${APP_HOME}/logs" /etc/kom/kubeconfigs && \
    chown -R "${APP_USER}:${APP_GROUP}" "${APP_HOME}" /etc/kom

WORKDIR ${APP_HOME}

COPY --from=builder --chown=${APP_USER}:${APP_GROUP} /out/kom-mcp ${APP_HOME}/kom-mcp
# 将 kubectl 二进制复制到 /usr/local/bin，使其在 PATH 中可被直接调用
# Copy kubectl binary so it's available on PATH inside the container
COPY --from=builder /out/kubectl /usr/local/bin/kubectl

USER ${APP_USER}

EXPOSE ${PORT}

ENV KOM_MCP_PORT=${PORT} \
    KOM_KUBECONFIG_DIR=/etc/kom/kubeconfigs \
    TZ=Asia/Shanghai

HEALTHCHECK --interval=30s --timeout=10s --retries=3 \
    CMD nc -z 127.0.0.1 "${KOM_MCP_PORT}" || exit 1

ENTRYPOINT ["/app/kom-mcp"]
