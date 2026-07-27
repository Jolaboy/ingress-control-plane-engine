# =============================================================================
# Multi-stage Dockerfile for the Ingress Control Plane Engine controller.
# Stage 1: Build the Go binary in a full Go toolchain image.
# Stage 2: Copy only the binary into a minimal distroless runtime image.
# =============================================================================

# ---------------------------------------------------------------------------
# Stage 1: Builder
# ---------------------------------------------------------------------------
FROM golang:1.22-alpine AS builder

ARG BINARY=control-plane
ARG VERSION=dev

# Install CA certs and git (needed for go module downloads over HTTPS)
RUN apk add --no-cache ca-certificates git

WORKDIR /workspace

# Cache module downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

# Copy full source
COPY . .

# Build a statically linked binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /workspace/bin/${BINARY} \
    ./cmd/controller

# ---------------------------------------------------------------------------
# Stage 2: Runtime (distroless — no shell, minimal attack surface)
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

ARG BINARY=control-plane

# Copy CA certs for TLS connections to Kubernetes API server
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the compiled binary
COPY --from=builder /workspace/bin/${BINARY} /controller

# Run as non-root UID 65532 (distroless nonroot)
USER 65532:65532

EXPOSE 18000 8080 8081

ENTRYPOINT ["/controller"]
