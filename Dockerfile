# Multi-stage build that produces a ~7 MB distroless image.
# Use case: run `a2abridge directory` as a daemon in a container, or
# attach `a2abridge worker` to a sidecar — without dragging glibc/Alpine
# CVEs into your stack.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Cache module fetch separately so source-only edits skip re-download.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /a2abridge ./cmd/a2abridge

# distroless/static is the smallest base that has /etc/ssl/certs (needed
# by HTTPS calls to GitHub for `a2abridge update`) and a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /a2abridge /a2abridge

# Default command: run the directory daemon on all interfaces inside the
# container. Override at `docker run` time for `bridge`, `worker`, etc.
EXPOSE 7777
ENTRYPOINT ["/a2abridge"]
CMD ["directory", "-addr", "0.0.0.0:7777"]
