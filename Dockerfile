# switchboard: one static Go binary on distroless.
#
# Base images are build args so a build without access to Docker Hub / gcr.io
# can use a mirror:
#   docker build --build-arg GO_IMAGE=<mirror>/golang:1.26 \
#                --build-arg RUNTIME_IMAGE=<mirror>/distroless/static-debian12:nonroot .
ARG GO_IMAGE=golang:1.26
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/switchboard ./cmd/switchboard

FROM ${RUNTIME_IMAGE}
COPY --from=build /out/switchboard /switchboard
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/switchboard"]
