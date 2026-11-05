# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/flaresolverr-go .

FROM debian:trixie-slim
# Debian's chromium ships SwiftShader, so WebGL works without a GPU.
# Alpine's build lacks it, and the Cloudflare challenge stalls without WebGL.
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation tzdata ca-certificates \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc/* /usr/share/man/* \
 && useradd -m -u 1000 -s /usr/sbin/nologin app
COPY --from=build /out/flaresolverr-go /usr/local/bin/flaresolverr-go
USER app
ENV PORT=8191 \
    LOG_LEVEL=info \
    TZ=UTC \
    HOME=/home/app \
    BROWSER_PATH=/usr/bin/chromium
EXPOSE 8191
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/flaresolverr-go", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/flaresolverr-go"]
