# syntax=docker/dockerfile:1.7
#
# Build stages:
#   ort      - fetches the ONNX Runtime shared library once, for reuse below
#   builder  - compiles the server with CGO enabled
#   runtime  - the production image (default target)
#   dev      - the toolchain image: go, linters, goose. Used for tests.
#
# The dev image exists so that nobody needs a CGO toolchain plus ONNX Runtime on
# their host. On Windows in particular that is a fight not worth having.

ARG GO_VERSION=1.23
ARG DEBIAN_RELEASE=bookworm
# ONNX Runtime's C API version tracks its minor version: 1.28 exposes API 28.
# onnxruntime_go vendors a header pinned to a specific API version and refuses
# to initialise against an older library, so these two move together. Bumping
# the Go binding means checking this number.
ARG ORT_VERSION=1.28.0

# -----------------------------------------------------------------------------
# ort: download and unpack ONNX Runtime
# -----------------------------------------------------------------------------
FROM debian:${DEBIAN_RELEASE}-slim AS ort

ARG ORT_VERSION
ARG TARGETARCH

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) ort_arch=x64 ;; \
      arm64) ort_arch=aarch64 ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ort_arch}-${ORT_VERSION}.tgz"; \
    mkdir -p /opt/onnxruntime; \
    tar -xzf /tmp/ort.tgz -C /opt/onnxruntime --strip-components=1; \
    rm /tmp/ort.tgz; \
    test -f /opt/onnxruntime/lib/libonnxruntime.so

# -----------------------------------------------------------------------------
# builder: compile the server
# -----------------------------------------------------------------------------
FROM golang:${GO_VERSION}-${DEBIAN_RELEASE} AS builder

WORKDIR /src
ENV CGO_ENABLED=1

COPY --from=ort /opt/onnxruntime /opt/onnxruntime

# Dependencies first so source edits do not invalidate the download layer.
# go.sum may not exist on a fresh checkout; the glob tolerates that.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# -----------------------------------------------------------------------------
# runtime: the shipped image
# -----------------------------------------------------------------------------
FROM debian:${DEBIAN_RELEASE}-slim AS runtime

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 10001 --user-group --no-create-home app

# Register the library with the dynamic linker so the binary resolves it without
# an LD_LIBRARY_PATH override — an override tends to mask a broken install.
COPY --from=ort /opt/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/
RUN ldconfig && ldconfig -p | grep -q libonnxruntime

COPY --from=builder /out/server /usr/local/bin/server

# Models are mounted, never baked in: they are large and separately licensed.
RUN mkdir -p /models && chown app:app /models
VOLUME ["/models"]

USER app
EXPOSE 8080

# The server probes itself, so the image needs no curl or wget.
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=5 \
  CMD ["/usr/local/bin/server", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/server"]

# -----------------------------------------------------------------------------
# dev: toolchain image for tests, linting, and migrations
# -----------------------------------------------------------------------------
FROM golang:${GO_VERSION}-${DEBIAN_RELEASE} AS dev

ENV CGO_ENABLED=1 \
    GOFLAGS=-buildvcs=false \
    LV_ONNXRUNTIME_LIB=/usr/local/lib/libonnxruntime.so

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git postgresql-client \
 && rm -rf /var/lib/apt/lists/*

COPY --from=ort /opt/onnxruntime /opt/onnxruntime
# -a preserves the symlink chain (libonnxruntime.so -> .so.1 -> .so.1.19.2).
# A plain cp dereferences it, which both wastes ~30 MB on duplicate copies and
# makes ldconfig complain that .so.1 is not a symbolic link.
RUN cp -a /opt/onnxruntime/lib/libonnxruntime.so* /usr/local/lib/ \
 && ldconfig \
 && ldconfig -p | grep -q libonnxruntime

# gotestsum turns `go test -json` into JUnit XML.
#
# Only CI needs it, but it lives here rather than being fetched per build: a
# pipeline that downloads a tool on every run is a pipeline that breaks when the
# network does, and pinning it in the image is what makes the version the same
# for everyone.
RUN go install mvdan.cc/gofumpt@v0.7.0 \
 && go install github.com/pressly/goose/v3/cmd/goose@v3.22.1 \
 && go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.61.0 \
 && go install gotest.tools/gotestsum@v1.12.0

WORKDIR /src
CMD ["go", "test", "./...", "-race"]
