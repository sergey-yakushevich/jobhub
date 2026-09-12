# Build on the host's own architecture and cross-compile to the target. Go
# needs no emulation to do this, and CGO is already off. Building the amd64
# image from an arm64 Mac used to run the whole toolchain under QEMU, which
# took over twenty minutes; this takes seconds.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o /jobhub ./cmd/jobhub

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D app
USER app
WORKDIR /data
ENV DB_PATH=/data/jobs.db PORT=8080
COPY --from=build /jobhub /usr/local/bin/jobhub
EXPOSE 8080
CMD ["jobhub"]
