# Stage 1: Build operator binary
ARG KRAKEND_VERSION=2.13
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
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -a -o manager cmd/main.go

# Stage 2: Extract KrakenD CE binary and musl libraries
FROM krakend/krakend:${KRAKEND_VERSION} AS krakend
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
USER 65532:65532

ENTRYPOINT ["/manager"]
