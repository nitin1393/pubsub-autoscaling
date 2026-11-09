# Build
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/vpa-gate ./cmd

# Run: static distroless, non-root
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/vpa-gate /vpa-gate
USER 65532:65532
ENTRYPOINT ["/vpa-gate"]
