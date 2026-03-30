ARG BASE_IMAGE=ghcr.io/ggml-org/llama.cpp
ARG BASE_TAG=server-cuda

FROM golang:1.26.1-bookworm AS compose-config-gen-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/compose-config-gen ./cmd/compose-config-gen
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/compose-config-gen ./cmd/compose-config-gen

FROM node:22-bookworm-slim AS ui-builder

WORKDIR /src/ui-svelte
COPY ui-svelte/package.json ui-svelte/package-lock.json ./
RUN npm ci
COPY ui-svelte ./
RUN mkdir -p /src/proxy && npm run build

FROM golang:1.26.1-bookworm AS llama-swap-builder

ARG GIT_HASH=unknown
ARG BUILD_DATE=unknown
ARG LS_VER=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY event ./event
COPY proxy ./proxy
COPY llama-swap.go ./
COPY --from=ui-builder /src/proxy/ui_dist ./proxy/ui_dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X main.version=${LS_VER} -X main.commit=${GIT_HASH} -X main.date=${BUILD_DATE}" \
    -o /out/llama-swap .

ARG BASE_IMAGE
ARG BASE_TAG
FROM ${BASE_IMAGE}:${BASE_TAG}

# has to be after the FROM
ARG LS_VER=dev
ARG LS_REPO=mostlygeek/llama-swap
ARG GIT_HASH=unknown
ARG BUILD_DATE=unknown

# Set default UID/GID arguments
ARG UID=10001
ARG GID=10001
ARG USER_HOME=/app

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

RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates && rm -rf /var/lib/apt/lists/*

RUN curl -L https://download.docker.com/linux/static/stable/x86_64/docker-28.0.1.tgz \
      | tar -xz --strip-components=1 -C /usr/local/bin docker/docker

RUN curl -L -o /usr/local/bin/docker-compose \
      https://github.com/docker/compose/releases/download/v2.35.1/docker-compose-linux-x86_64 \
      && chmod +x /usr/local/bin/docker-compose

COPY --from=compose-config-gen-builder /out/compose-config-gen /app/compose-config-gen
COPY --from=llama-swap-builder /out/llama-swap /app/llama-swap
COPY --chown=$UID:$GID docker/config.example.yaml /app/config.yaml

LABEL org.opencontainers.image.source="https://github.com/${LS_REPO}" \
      org.opencontainers.image.revision="${GIT_HASH}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.version="${LS_VER}"

# Switch user
USER $UID:$GID

WORKDIR /app

# Add /app to PATH
ENV PATH="/app:${PATH}"

HEALTHCHECK CMD curl -f http://localhost:8080/ || exit 1
ENTRYPOINT [ "/app/llama-swap", "-config", "/app/config.yaml" ]
