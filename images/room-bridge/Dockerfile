# Static binary on distroless: no shell, no package manager, uid 65532. The
# bridge's pod securityContext (restricted PSS) overrides the uid anyway.
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/Smana/agent-platform/internal/version.Version=${VERSION}" \
      -o /out/room-bridge ./cmd/room-bridge

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/room-bridge /room-bridge
USER nonroot:nonroot
ENTRYPOINT ["/room-bridge"]
