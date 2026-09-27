# Aether Edge agent image (docs/platform/aether-edge.md). Built for linux/amd64 and linux/arm64 by
# .github/workflows/edge.yml; locally: docker build -f backend/edge.Dockerfile backend
#
# Only cmd/aether-edge is compiled: the agent imports no server code (TestAgentDependencies).
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/aether-edge ./cmd/aether-edge
COPY internal/edge ./internal/edge
COPY internal/tuyalocal ./internal/tuyalocal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X aether/backend/internal/edge.Version=${VERSION}" -o /out/aether-edge ./cmd/aether-edge

# distroless/static (nonroot variant), pinned by digest: no shell, no package manager, CA certificates for the HTTPS
# configuration pull. Refresh the digest with: docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.title="aether-edge" \
      org.opencontainers.image.description="Aether Edge: local Tuya Wi-Fi agent for the Aether IoT platform" \
      org.opencontainers.image.source="https://github.com/panudet-24mb/aether" \
      org.opencontainers.image.licenses="NOASSERTION" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/aether-edge /aether-edge
USER 10001:10001
HEALTHCHECK --interval=60s --timeout=5s --start-period=90s --retries=3 CMD ["/aether-edge", "health"]
ENTRYPOINT ["/aether-edge"]
