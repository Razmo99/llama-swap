ARG BASE_IMAGE=ghcr.io/ggml-org/llama.cpp
ARG BASE_TAG=server-cuda

FROM golang:1.26.1-bookworm AS compose-config-gen-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/compose-config-gen ./cmd/compose-config-gen
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/compose-config-gen ./cmd/compose-config-gen

ARG BASE_IMAGE
ARG BASE_TAG
FROM ${BASE_IMAGE}:${BASE_TAG}

# has to be after the FROM
ARG LS_VER=170
ARG LS_REPO=mostlygeek/llama-swap

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

RUN \
    curl -LO "https://github.com/${LS_REPO}/releases/download/v${LS_VER}/llama-swap_${LS_VER}_linux_amd64.tar.gz" && \
    tar -zxf "llama-swap_${LS_VER}_linux_amd64.tar.gz" && \
    rm "llama-swap_${LS_VER}_linux_amd64.tar.gz"

COPY --from=compose-config-gen-builder /out/compose-config-gen /app/compose-config-gen
COPY --chown=$UID:$GID config.example.yaml /app/config.yaml

# Switch user
USER $UID:$GID

WORKDIR /app

# Add /app to PATH
ENV PATH="/app:${PATH}"

HEALTHCHECK CMD curl -f http://localhost:8080/ || exit 1
ENTRYPOINT [ "/app/llama-swap", "-config", "/app/config.yaml" ]
