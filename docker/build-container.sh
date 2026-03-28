#!/bin/bash

set -euo pipefail

cd $(dirname "$0")

# use this to test locally, example:
# GITHUB_TOKEN=$(gh auth token) LOG_DEBUG=1 DEBUG_ABORT_BUILD=1 ./docker/build-container.sh rocm
# you need read:package scope on the token. Generate a personal access token with
# the scopes: gist, read:org, repo, write:packages
# then: gh auth login (and copy/paste the new token)

LOG_DEBUG=${LOG_DEBUG:-0}
DEBUG_ABORT_BUILD=${DEBUG_ABORT_BUILD:-}

log_debug() {
    if [ "$LOG_DEBUG" = "1" ]; then
        echo "[DEBUG] $*"
    fi
}

log_info() {
    echo "[INFO] $*"
}

resolve_repo_slug() {
    if [[ -n "${GITHUB_REPOSITORY:-}" ]]; then
        echo "${GITHUB_REPOSITORY}"
        return
    fi

    local remote_url
    remote_url=$(git -C .. remote get-url origin 2>/dev/null || true)
    if [[ -z "${remote_url}" ]]; then
        return
    fi

    echo "${remote_url}" \
        | sed -E 's#^https://github.com/##; s#^git@github.com:##; s#\.git$##'
}

ARCH=$1
PUSH_IMAGES=${2:-false}

# List of allowed architectures
ALLOWED_ARCHS=("intel" "vulkan" "musa" "cuda" "cuda13" "cpu" "rocm")

# Check if ARCH is in the allowed list
if [[ ! " ${ALLOWED_ARCHS[@]} " =~ " ${ARCH} " ]]; then
  log_info "Error: ARCH must be one of the following: ${ALLOWED_ARCHS[@]}"
  exit 1
fi

# Check if GITHUB_TOKEN is set and not empty
if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  log_info "Error: GITHUB_TOKEN is not set or is empty."
  exit 1
fi

# Set llama.cpp base image, customizable using the BASE_LLAMACPP_IMAGE environment
# variable, this permits testing with forked llama.cpp repositories
BASE_IMAGE=${BASE_LLAMACPP_IMAGE:-ghcr.io/ggml-org/llama.cpp}
SD_IMAGE=${BASE_SDCPP_IMAGE:-ghcr.io/leejet/stable-diffusion.cpp}
BUILD_PLATFORM=${BUILD_PLATFORM:-linux/amd64}

# Set llama-swap repository from the current GitHub repository when available,
# otherwise fall back to the local origin remote.
LS_REPO=${LLAMA_SWAP_REPO_NAME:-$(resolve_repo_slug)}
if [[ -z "${LS_REPO}" ]]; then
  LS_REPO="Razmo99/llama-swap"
fi
LS_REPO_LOWER=$(echo "${LS_REPO}" | tr '[:upper:]' '[:lower:]')
LS_DOWNLOAD_REPO=${LS_REPO}

# the most recent llama-swap tag
# have to strip out the 'v' due to .tar.gz file naming
release_response_file=$(mktemp)
release_status=$(curl -s -o "${release_response_file}" -w "%{http_code}" "https://api.github.com/repos/${LS_REPO}/releases/latest")
case "${release_status}" in
  200)
    LS_VER=$(jq -r '.tag_name // empty' "${release_response_file}" | sed 's/v//')
    if [[ -z "${LS_VER}" ]]; then
      log_info "Error: latest release response for ${LS_REPO} did not include tag_name."
      rm -f "${release_response_file}"
      exit 1
    fi
    ;;
  404)
    log_info "No releases found for ${LS_REPO}; falling back to upstream release artifacts."
    upstream_response_file=$(mktemp)
    upstream_status=$(curl -s -o "${upstream_response_file}" -w "%{http_code}" "https://api.github.com/repos/mostlygeek/llama-swap/releases/latest")
    if [[ "${upstream_status}" != "200" ]]; then
      log_info "Error: failed to resolve latest upstream release (HTTP ${upstream_status})."
      if jq -e '.message' "${upstream_response_file}" >/dev/null 2>&1; then
        log_info "GitHub API error: $(jq -r '.message' "${upstream_response_file}")"
      fi
      rm -f "${release_response_file}" "${upstream_response_file}"
      exit 1
    fi
    LS_VER=$(jq -r '.tag_name // empty' "${upstream_response_file}" | sed 's/v//')
    if [[ -z "${LS_VER}" ]]; then
      log_info "Error: latest upstream release did not include tag_name."
      rm -f "${release_response_file}" "${upstream_response_file}"
      exit 1
    fi
    LS_DOWNLOAD_REPO="mostlygeek/llama-swap"
    rm -f "${upstream_response_file}"
    ;;
  *)
    log_info "Error: failed to resolve latest release for ${LS_REPO} (HTTP ${release_status})."
    if jq -e '.message' "${release_response_file}" >/dev/null 2>&1; then
      log_info "GitHub API error: $(jq -r '.message' "${release_response_file}")"
    fi
    rm -f "${release_response_file}"
    exit 1
    ;;
esac
rm -f "${release_response_file}"

# Fetches the most recent llama.cpp tag matching the given regex.
# Optionally filters tags by manifest architecture.
# $1 - tag_prefix (e.g., "server" or "server-vulkan")
# $2 - tag_regex (e.g., "^server-b[0-9]+$")
# $3 - required_arch (optional, e.g., "amd64")
# Returns: the version number extracted from the tag
fetch_llama_tag() {
    local tag_prefix=$1
    local tag_regex=$2
    local required_arch=${3:-}
    local page=1
    local per_page=100

    while true; do
        log_debug "Fetching page $page for tag prefix: $tag_prefix"

        local response=$(curl -s -H "Authorization: Bearer $GITHUB_TOKEN" \
            "https://api.github.com/users/ggml-org/packages/container/llama.cpp/versions?per_page=${per_page}&page=${page}")

        # Check for API errors
        if echo "$response" | jq -e '.message' > /dev/null 2>&1; then
            local error_msg=$(echo "$response" | jq -r '.message')
            log_info "GitHub API error: $error_msg"
            return 1
        fi

        # Check if response is empty array (no more pages)
        if [ "$(echo "$response" | jq 'length')" -eq 0 ]; then
            log_debug "No more pages (empty response)"
            return 1
        fi

        while IFS= read -r found_tag; do
            [[ -z "$found_tag" ]] && continue

            if [[ -n "$required_arch" ]]; then
                local manifest
                manifest=$(docker manifest inspect --verbose "${BASE_IMAGE}:${found_tag}" 2>/dev/null || true)
                local manifest_arch
                local manifest_os
                manifest_arch=$(echo "$manifest" | jq -r '.Descriptor.platform.architecture // empty' 2>/dev/null || true)
                manifest_os=$(echo "$manifest" | jq -r '.Descriptor.platform.os // empty' 2>/dev/null || true)

                if [[ "$manifest_arch" != "$required_arch" || "$manifest_os" != "linux" ]]; then
                    log_debug "Skipping tag $found_tag with platform ${manifest_os:-unknown}/${manifest_arch:-unknown}"
                    continue
                fi
            fi

            log_debug "Found tag: $found_tag on page $page"
            echo "$found_tag" | awk -F '-' '{print $NF}'
            return 0
        done < <(
            echo "$response" \
                | jq -r '.[] | .metadata.container.tags[]?' \
                | grep -E "$tag_regex" \
                | sort -Vr
        )

        page=$((page + 1))

        # Safety limit to prevent infinite loops
        if [ $page -gt 50 ]; then
            log_info "Reached pagination safety limit (50 pages)"
            return 1
        fi
    done
}

if [ "$ARCH" == "cpu" ]; then
    LCPP_TAG=$(fetch_llama_tag "server" '^server-b[0-9]+$' "amd64")
    BASE_TAG=server-${LCPP_TAG}
else
    LCPP_TAG=$(fetch_llama_tag "server-${ARCH}" "^server-${ARCH}-b[0-9]+$")
    BASE_TAG=server-${ARCH}-${LCPP_TAG}
fi

SD_TAG=master-${ARCH}

# Abort if LCPP_TAG is empty.
if [[ -z "$LCPP_TAG" ]]; then
    log_info "Abort: Could not find llama-server container for arch: $ARCH"
    exit 1
else
    log_info "LCPP_TAG: $LCPP_TAG"
fi

if [[ ! -z "$DEBUG_ABORT_BUILD" ]]; then
    log_info "Abort: DEBUG_ABORT_BUILD set"
    exit 0
fi

GIT_HASH=$(git -C .. rev-parse --short HEAD)
if [[ -n "$(git -C .. status --porcelain)" ]]; then
  GIT_HASH="${GIT_HASH}+"
fi
BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

for CONTAINER_TYPE in non-root root; do
  CONTAINER_TAG="ghcr.io/${LS_REPO_LOWER}:v${LS_VER}-${ARCH}-${LCPP_TAG}"
  CONTAINER_LATEST="ghcr.io/${LS_REPO_LOWER}:${ARCH}"
  USER_UID=0
  USER_GID=0
  USER_HOME=/root

  if [ "$CONTAINER_TYPE" == "non-root" ]; then
    CONTAINER_TAG="${CONTAINER_TAG}-non-root"
    CONTAINER_LATEST="${CONTAINER_LATEST}-non-root"
    USER_UID=10001
    USER_GID=10001
    USER_HOME=/app
  fi

  log_info "Building $CONTAINER_TYPE $CONTAINER_TAG $LS_VER"
  docker build --platform ${BUILD_PLATFORM} --provenance=false -f llama-swap.Containerfile --build-arg BASE_TAG=${BASE_TAG} --build-arg UID=${USER_UID} \
    --build-arg LS_VER=${LS_VER} --build-arg LS_REPO=${LS_DOWNLOAD_REPO} --build-arg GID=${USER_GID} \
    --build-arg USER_HOME=${USER_HOME} --build-arg BASE_IMAGE=${BASE_IMAGE} \
    -t ${CONTAINER_TAG} -t ${CONTAINER_LATEST} .

  # For architectures with stable-diffusion.cpp support, layer sd-server on top
  case "$ARCH" in
    "musa" | "vulkan")
      log_info "Adding sd-server to $CONTAINER_TAG"
      docker build --platform ${BUILD_PLATFORM} --provenance=false -f llama-swap-sd.Containerfile \
        --build-arg BASE=${CONTAINER_TAG} \
        --build-arg SD_IMAGE=${SD_IMAGE} --build-arg SD_TAG=${SD_TAG} \
        --build-arg UID=${USER_UID} --build-arg GID=${USER_GID} \
        -t ${CONTAINER_TAG} -t ${CONTAINER_LATEST} . ;;
  esac

  if [ "$PUSH_IMAGES" == "true" ]; then
    docker push ${CONTAINER_TAG}
    docker push ${CONTAINER_LATEST}
  fi
done
