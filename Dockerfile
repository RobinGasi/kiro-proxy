# syntax=docker/dockerfile:1

# --- build stage ---
# Build on the native runner arch and cross-compile for the target (no QEMU).
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

# Cache modules first.
COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .

ARG VERSION=0.0.0-dev
ARG TARGETOS
ARG TARGETARCH
# CGO disabled -> fully static binary (modernc.org/sqlite is pure Go).
ENV CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X github.com/RobinGasi/kiro-proxy/internal/config.AppVersion=${VERSION}" \
    -o /out/kiro-proxy ./cmd/kiro-proxy
# Empty dir for the state volume; see Dockerfile.goreleaser.
RUN mkdir /out/state

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/kiro-proxy /app/kiro-proxy
COPY --from=build --chown=nonroot:nonroot /out/state /state
EXPOSE 8442
USER nonroot:nonroot
ENTRYPOINT ["/app/kiro-proxy"]
