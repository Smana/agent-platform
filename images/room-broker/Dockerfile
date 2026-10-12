# Static binary on distroless: no shell, no package manager, uid 65532. The
# broker's pod securityContext (restricted PSS) overrides the uid anyway.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X github.com/Smana/agent-platform/internal/version.Version=${VERSION}" \
      -o /out/room-broker ./cmd/room-broker

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/room-broker /room-broker
USER nonroot:nonroot
ENTRYPOINT ["/room-broker"]
