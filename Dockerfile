# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.26 AS build
WORKDIR /src

# Cache modules first.
COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .

ARG VERSION=0.0.0-dev
# CGO disabled -> fully static binary (modernc.org/sqlite is pure Go).
ENV CGO_ENABLED=0
RUN go build -trimpath \
    -ldflags="-s -w -X github.com/RobinGasi/kiro-proxy/internal/config.AppVersion=${VERSION}" \
    -o /out/kiro-proxy ./cmd/kiro-proxy

# --- runtime stage ---
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/kiro-proxy /app/kiro-proxy
EXPOSE 8442
USER nonroot:nonroot
ENTRYPOINT ["/app/kiro-proxy"]
