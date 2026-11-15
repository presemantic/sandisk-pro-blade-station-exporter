FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/exporter ./cmd/sandisk-pro-blade-station-exporter

FROM scratch
ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/presemantic/sandisk-pro-blade-station-exporter" \
      org.opencontainers.image.description="Linux HID telemetry and opt-in cooling control for SanDisk PRO-BLADE Station" \
      org.opencontainers.image.version=$VERSION
COPY --from=build /out/exporter /exporter
USER 0:0
EXPOSE 9908
ENTRYPOINT ["/exporter"]
