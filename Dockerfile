# Builds either binary from the same source: --build-arg ROLE=prober|viewer.
#
# The build stage pins Go 1.21 on purpose: that is the module's declared floor, so
# a broken floor claim fails here rather than in someone else's CI.
FROM golang:1.21-alpine AS build

ARG ROLE=prober
ARG TARGETOS=linux
ARG TARGETARCH
# Release version, stamped into the binary and reported at startup and by /metrics.
ARG VERSION=0.1.1-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH:-$(go env GOARCH)} \
    go build -trimpath \
      -ldflags="-s -w -X pulse/internal/build.Version=${VERSION}" \
      -o /out/pulse ./cmd/${ROLE}

# Pre-create the history directory with the runtime user's ownership: Docker
# initialises an empty named volume from the image path, so sqlite can write to it.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static:nonroot

COPY --from=build --chown=65532:65532 /out/pulse /usr/local/bin/pulse
COPY --from=build --chown=65532:65532 /out/data /var/lib/pulse

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pulse"]
