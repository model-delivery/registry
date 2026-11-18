# syntax=docker/dockerfile:1
# The build stage runs on the builder's own platform and cross-compiles, so
# multi-arch images build at native speed. The binary has no C dependencies.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/modelctl /modelctl
COPY --from=build --chown=65532:65532 /out/data /data
ENV REGISTRY_DATA=/data
VOLUME /data
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/modelctl"]
CMD ["serve"]
