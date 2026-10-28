# Static binary on distroless: no shell, no package manager, uid 65532. The
# pod securityContext (restricted PSS) overrides the uid anyway.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X github.com/Smana/maku/internal/version.Version=${VERSION}" \
      -o /out/factory-slack-app ./cmd/factory-slack-app

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/factory-slack-app /factory-slack-app
USER nonroot:nonroot
ENTRYPOINT ["/factory-slack-app"]
