# syntax=docker/dockerfile:1

# Which generator to build, e.g. book, logs, or random. Selects the package
# under ./cmd/${CMD}. The data files each generator needs are baked in via
# //go:embed, so no runtime assets are required.
ARG GO_VERSION=1.25

# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH. Because CGO is disabled
# the cross-compile is a plain GOARCH switch, so a multi-arch build (linux/amd64 + linux/arm64) never
# pays for QEMU emulation of the toolchain - each target is compiled natively on the amd64 runner.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG CMD
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules first so the layer is cached across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/generator ./cmd/${CMD}

# The final stage is naturally the TARGETPLATFORM; distroless/static is a multi-arch base, so buildx
# selects the matching arch for each platform in the build.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/generator /generator

ENTRYPOINT ["/generator"]
