# Building the Karpenter controller image (fork)

This fork builds the controller image with [`ko`](https://ko.build) (same toolchain as
`hack/release/common.sh` and the `container-build.yaml` workflow). There is no Dockerfile —
`ko` compiles `./cmd/controller` and layers it onto the base image from `.ko.yaml`
(`mcr.microsoft.com/azurelinux/distroless/base:3.0`).

## Prerequisites

- `ko` (`go install github.com/google/ko@latest`)
- Go matching `go.mod` (currently `1.27.2`; `GOTOOLCHAIN=auto` downloads it)
- Docker Desktop running (for `--local` builds, and for `az acr login`)
- `az acr login -n <acr>` first if pushing to ACR

## Pure-Go build config (required on macOS)

Upstream's `.ko.yaml` sets `CGO_ENABLED=1`: their release builds use Microsoft Go with
`hack/go-crossbuild.sh` (Linux cross-compilers) for FIPS crypto. That fails on macOS, and we don't
need FIPS, so build a static pure-Go binary from a copy of the config instead of editing
`.ko.yaml` (keeps upstream syncs conflict-free):

```bash
export KO_CONFIG_PATH=$(mktemp -d)
sed 's/CGO_ENABLED=1/CGO_ENABLED=0/' .ko.yaml > "$KO_CONFIG_PATH/.ko.yaml"
```

The commands below assume `KO_CONFIG_PATH` is set this way.

> **Building an image for a cluster? Use [Push to ACR](#push-to-acr-multi-arch) below.**
> `--local` cannot produce a multi-arch image (see the note in that section), so a `--local`
> image runs on only one node architecture and crashes on the other.

## Build locally (single-arch, local testing only)

```bash
DOCKER_HOST=$(docker context inspect --format '{{.Endpoints.docker.Host}}') \
SOURCE_DATE_EPOCH=$(date +%s) KO_DATA_DATE_EPOCH=$(date +%s) \
KO_DOCKER_REPO=cicdprodacr.azurecr.io/karpenter \
ko build -B --sbom none --local --platform linux/arm64 \
  -t "$(git ls-remote --tags --refs upstream 'v*' | awk -F/ '{print $NF}' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 | sed 's/^v//')" \
  ./cmd/controller
```

Produces a single-arch `cicdprodacr.azurecr.io/karpenter/controller:<latest-release-tag>` in
your local Docker daemon (e.g. `...:1.12.1`). Set `--platform` to match where you run it
(`linux/arm64` on Apple Silicon, `linux/amd64` otherwise). `--platform all` does **not** give a
multi-arch image with `--local` — see the note under [Push to ACR](#push-to-acr-multi-arch).

### What each part does

- `DOCKER_HOST=$(docker context inspect ...)` — points `ko` at Docker Desktop's socket. `ko`
  doesn't read Docker contexts and otherwise defaults to `/var/run/docker.sock`, which Docker
  Desktop doesn't expose by default. This derives the socket from your current context.
- `SOURCE_DATE_EPOCH` / `KO_DATA_DATE_EPOCH` — reproducible build timestamps.
- `KO_DOCKER_REPO` — target repo. `ko` appends `/controller`, so the image is
  `<repo>/controller:<tag>`.
- `-B` — bare (no extra md5 path segment under the repo).
- `--sbom none` — skip SBOM generation.
- `--local` — load into the local Docker daemon instead of pushing (single-arch only — the
  daemon-load path cannot hold a multi-platform index).
- `--platform` — target platform(s). `all` builds `linux/arm64` + `linux/amd64` from `.ko.yaml`
  when pushing to a registry; with `--local` only a single arch is materialized.
- `-t "$(git ls-remote ... | sed 's/^v//')"` — tag with the latest upstream release tag,
  `v` stripped (e.g. `v1.12.1` -> `1.12.1`). Swap for `$(git rev-parse --short=8 HEAD)` to tag
  with the commit SHA, or any fixed string.

## Push to ACR (multi-arch)

This is the path for any image that runs on a cluster. Drop `--local`, run
`az acr login -n cicdprodacr` first, and `ko build` pushes a proper multi-arch image index
(`linux/amd64` + `linux/arm64`) directly:

```bash
az acr login -n cicdprodacr
VERSION=$(git ls-remote --tags --refs upstream 'v*' | awk -F/ '{print $NF}' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)
GOFLAGS="-ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version=${VERSION}" \
SOURCE_DATE_EPOCH=$(date +%s) KO_DATA_DATE_EPOCH=$(date +%s) \
KO_DOCKER_REPO=cicdprodacr.azurecr.io/karpenter \
ko build -B --sbom none --platform all \
  -t "${VERSION}" -t "$(git rev-parse --short=8 HEAD)" \
  ./cmd/controller
```

Clusters reference the `v`-prefixed tag (e.g. `v1.14.5`); the commit-SHA tag is for traceability.
If the push fails with `UNAUTHORIZED`, `az acr login` didn't write Docker credentials (Docker not
running): use `az acr login -n <acr> --expose-token` and `docker login` /
`crane auth login` with user `00000000-0000-0000-0000-000000000000` and the returned token.

Verify both arches landed in the index:

```bash
docker buildx imagetools inspect cicdprodacr.azurecr.io/karpenter/controller:<tag>
```

> **Why `--local` can't do multi-arch.** `ko`'s `--local` loads into the Docker daemon via a
> single-image write that cannot represent a multi-platform index. With `--local --platform all`
> `ko` compiles both arches but the daemon load keeps only **one** (amd64), so the image fails
> to run on arm64 nodes (the pod crashes with an exec-format error). Pushing to a registry
> stores the full index, so the right arch is pulled on each node.

## CI

The `container-build.yaml` GitHub Actions workflow (on the `feat/container-build-workflow`
branch) runs the equivalent `ko publish` on push, publishing to the repo's `REGISTRY_URL`
secret tagged with the short commit SHA.
