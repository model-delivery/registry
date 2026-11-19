# syntax=docker/dockerfile:1
# Base images are pinned by digest so a build cannot silently pick up a
# different image under the same tag. Dependabot moves the pins.
# The build stage runs on the builder's own platform and cross-compiles, so
# multi-arch images build at native speed. The binary has no C dependencies.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/model-delivery/registry/internal/cli.Version=${VERSION}" \
      -o /out/modelctl ./cmd/modelctl
# The runtime image has no shell, so prepare the data directory here and
# hand it to the unprivileged user (uid 65532) the registry runs as.
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/modelctl /modelctl
COPY --from=build --chown=65532:65532 /out/data /data
LABEL org.opencontainers.image.vendor="Spectrum Labs LLC"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.source="https://github.com/model-delivery/registry"
LABEL org.opencontainers.image.url="https://model.delivery"
ENV REGISTRY_DATA=/data
VOLUME /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/modelctl"]
CMD ["serve"]
