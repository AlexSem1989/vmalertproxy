#!/usr/bin/env bash
# Builds the vmalertsproxy Docker image.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: ./build.sh [options]

Builds the vmalertsproxy Docker image. Unit tests run in Docker before the build,
so Go doesn't have to be installed locally.

Options:
  -i, --image NAME       Image repository (default: $IMAGE or vmalertsproxy)
  -t, --tag TAG          Image tag, also used as the app version
                         (default: $VERSION, then `git describe`, then dev)
  -p, --platform LIST    Target platforms, e.g. linux/amd64 or linux/amd64,linux/arm64
                         (default: the Docker host platform; with --push: linux/amd64,linux/arm64)
      --push             Push the image to the registry (run `docker login` first)
      --latest           Also tag the image as latest
      --skip-tests       Skip unit tests
      --e2e              Also run end-to-end tests from test/docker-compose.yml
  -h, --help             Show this help

Examples:
  ./build.sh
  ./build.sh -t 0.1.0 -p linux/amd64
  ./build.sh -i registry.example.com/monitoring/vmalertsproxy -t 0.1.0 --push --latest
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

log() {
  echo "==> $*"
}

cd "$(dirname "$0")"

image=${IMAGE:-vmalertsproxy}
tag=${VERSION:-}
platform=
push=false
latest=false
run_tests=true
run_e2e=false

while (($# > 0)); do
  case $1 in
    -i | --image)
      image=${2:?"$1 requires a value"}
      shift
      ;;
    -t | --tag)
      tag=${2:?"$1 requires a value"}
      shift
      ;;
    -p | --platform)
      platform=${2:?"$1 requires a value"}
      shift
      ;;
    --push) push=true ;;
    --latest) latest=true ;;
    --skip-tests) run_tests=false ;;
    --e2e) run_e2e=true ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) die "unknown option $1; see ./build.sh --help" ;;
  esac
  shift
done

command -v docker >/dev/null || die "docker is not installed"
docker buildx version >/dev/null 2>&1 || die "docker buildx is required"

revision=
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  revision=$(git rev-parse HEAD 2>/dev/null || true)
  if [[ -z $tag ]]; then
    tag=$(git describe --tags --always --dirty 2>/dev/null || true)
  fi
fi
tag=${tag:-dev}
[[ $tag =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid image tag $tag"

if [[ -z $platform ]] && $push; then
  platform=linux/amd64,linux/arm64
fi

compose=(docker compose -f test/docker-compose.yml)

if $run_tests; then
  log "Running unit tests"
  "${compose[@]}" run --rm unit
fi

if $run_e2e; then
  log "Running end-to-end tests"
  # Always remove the test stack, even if the tests fail.
  trap '"${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true' EXIT
  "${compose[@]}" run --rm --build e2e
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1
  trap - EXIT
fi

args=(
  --file Dockerfile
  --build-arg "VERSION=$tag"
  --label "org.opencontainers.image.title=vmalertsproxy"
  --label "org.opencontainers.image.version=$tag"
  --label "org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  --tag "$image:$tag"
)
[[ -n $revision ]] && args+=(--label "org.opencontainers.image.revision=$revision")
$latest && args+=(--tag "$image:latest")
[[ -n $platform ]] && args+=(--platform "$platform")
if $push; then
  args+=(--push)
else
  # Loading multi-platform images requires the containerd image store, which is the default in Docker Desktop.
  args+=(--load)
fi

log "Building $image:$tag${platform:+ for $platform}"
docker buildx build "${args[@]}" .

if $push; then
  log "Pushed $image:$tag"
else
  log "Built $image:$tag"
  docker image ls "$image:$tag" --format '    {{.Repository}}:{{.Tag}}  {{.Size}}'
fi
