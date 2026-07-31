# syntax=docker/dockerfile:1

# Which generator to build, e.g. book, logs, or random. Selects the package
# under ./cmd/${CMD}. The data files each generator needs are baked in via
# //go:embed, so no runtime assets are required.
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS build

ARG CMD

WORKDIR /src

# Download modules first so the layer is cached across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/generator ./cmd/${CMD}

FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/generator /generator

ENTRYPOINT ["/generator"]
