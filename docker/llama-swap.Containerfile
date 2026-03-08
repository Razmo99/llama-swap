ARG BASE_IMAGE=ghcr.io/ggml-org/llama.cpp
ARG BASE_TAG=server-cuda
ARG GO_IMAGE=golang:1.25
ARG NODE_IMAGE=node:22-bookworm-slim
ARG PODMAN_VERSION=v5.7.1
ARG DOCKER_COMPOSE_VERSION=v5.0.2

FROM ${NODE_IMAGE} AS ui-builder

WORKDIR /src/ui-svelte

COPY ui-svelte/package.json ui-svelte/package-lock.json ./
RUN npm ci

COPY ui-svelte/ ./
RUN npm run build

FROM ${GO_IMAGE} AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /src/proxy/ui_dist ./proxy/ui_dist

ARG VERSION=local
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -ldflags="-s -w -X main.commit=${COMMIT} -X main.version=${VERSION} -X main.date=${BUILD_DATE}" \
      -o /out/llama-swap . && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -ldflags="-s -w" \
      -o /out/compose-config-gen ./cmd/compose-config-gen

FROM alpine:3.22 AS podman-remote

ARG PODMAN_VERSION
ARG DOCKER_COMPOSE_VERSION

ADD --link https://github.com/containers/podman/releases/download/${PODMAN_VERSION}/podman-remote-static-linux_amd64.tar.gz /tmp/
ADD --chmod=755 --link https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-x86_64 /usr/local/bin/

RUN set -eux; \
    mv /usr/local/bin/docker-compose-linux-x86_64 /usr/local/bin/docker-compose; \
    cd /tmp; \
    tar xf podman-remote-static-linux_amd64.tar.gz; \
    mv bin/podman-remote-static-linux_amd64 /usr/local/bin/podman; \
    printf '%s\n' \
      '#!/bin/sh' \
      'if [ "$1" = "compose" ]; then' \
      '  shift' \
      '  exec /usr/local/bin/docker-compose "$@"' \
      'fi' \
      'exec /usr/local/bin/podman "$@"' > /usr/local/bin/docker; \
    chmod +x /usr/local/bin/podman /usr/local/bin/docker-compose /usr/local/bin/docker

FROM ${BASE_IMAGE}:${BASE_TAG}

# Set default UID/GID arguments
ARG UID=10001
ARG GID=10001
ARG USER_HOME=/app
ARG INSTALL_PODMAN=0

# Add user/group
ENV HOME=$USER_HOME
RUN if [ $UID -ne 0 ]; then \
      if [ $GID -ne 0 ]; then \
        groupadd --system --gid $GID app; \
      fi; \
      useradd --system --uid $UID --gid $GID \
      --home $USER_HOME app; \
    fi

# Handle paths
RUN mkdir --parents $HOME /app
RUN chown --recursive $UID:$GID $HOME /app

# Switch user
USER $UID:$GID

WORKDIR /app

# Add /app to PATH
ENV PATH="/app:${PATH}"

COPY --from=builder /out/llama-swap /app/llama-swap
COPY --from=builder /out/compose-config-gen /app/compose-config-gen
COPY --from=podman-remote /usr/local/bin/podman /usr/local/bin/podman
COPY --from=podman-remote /usr/local/bin/docker-compose /usr/local/bin/docker-compose
COPY --from=podman-remote /usr/local/bin/docker /usr/local/bin/docker

COPY --chown=$UID:$GID docker/config.example.yaml /app/config.yaml

HEALTHCHECK CMD curl -f http://localhost:8080/ || exit 1
ENTRYPOINT [ "/app/llama-swap", "-config", "/app/config.yaml" ]
