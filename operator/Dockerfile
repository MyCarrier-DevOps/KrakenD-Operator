# KrakenD CE image the validator binary comes from, pinned by digest so every
# build validates with the same binary. Change it together with
# ValidatorVersion in internal/configcheck and the gin version in go.mod.
ARG KRAKEND_IMAGE=docker.io/library/krakend:2.13.11@sha256:26eb32dbb0e679c62170b21b4efb0729dc00262afd98fe6210110dbbbdd6425f

# Stage 1: Build operator binary
FROM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -a -o manager ./cmd

# Stage 2: Extract KrakenD CE binary and musl libraries
FROM ${KRAKEND_IMAGE} AS krakend
# Collect musl libraries into a staging directory so we can copy them
# architecture-independently (x86_64 has ld-musl-x86_64, arm64 has ld-musl-aarch64).
RUN mkdir -p /staging/lib && cp /lib/ld-musl-*.so.1 /lib/libc.musl-*.so.1 /staging/lib/

# Stage 3: Final distroless image
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=krakend /usr/bin/krakend /usr/local/bin/krakend
# KrakenD binary is dynamically linked against musl; copy the dynamic linker
# and libc from the Alpine-based krakend image.
COPY --from=krakend /staging/lib/ /lib/
# Apache 2.0 requires redistributions to carry the license and notices; this
# covers the manager binary's Go dependencies, KrakenD CE, and musl.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
USER 65532:65532

ENTRYPOINT ["/manager"]
