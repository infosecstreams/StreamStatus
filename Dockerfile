# Use an intermediate container for initial building
FROM golang:latest AS builder
RUN sed -i -e 's/bookworm-updates/bookworm-updates sid/' /etc/apt/sources.list.d/debian.sources && apt-get update && apt-get install -y upx ca-certificates --no-install-recommends && apt-get clean && rm -rf /var/lib/apt/lists/*
# RUN apt update && apt install -y upx ca-certificates --no-install-recommends && apt clean && rm -rf /var/lib/apt/lists/*

# Use go modules and don't let go packages call C code
ENV GO111MODULE=on CGO_ENABLED=0
WORKDIR /build
COPY src/ /build/
ARG VERSION
ENV VERSION=${VERSION:-unknown}
RUN GOOS=linux GOARCH=amd64 \
    go build \
    -mod=vendor \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o StreamStatus ./...

# Compress the binary and verify the output using UPX
# h/t @FiloSottile/Filippo Valsorda: https://blog.filippo.io/shrink-your-go-binaries-with-this-one-weird-trick/
RUN upx -v --lzma --best /build/StreamStatus && upx -vt /build/StreamStatus
RUN mkdir /data

# Copy the contents of /dist to the root of a scratch containter
FROM scratch
COPY --chown=0:0 --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --chown=0:0 --from=builder /build/StreamStatus /
WORKDIR /
ENTRYPOINT ["/StreamStatus"]
LABEL org.opencontainers.image.authors='goproslowyo@gmail.com'
LABEL org.opencontainers.image.description="Twitch Stream Status"
LABEL org.opencontainers.image.licenses='Apache-2.0'
LABEL org.opencontainers.image.source='https://github.com/infosecstreams/StreamStatus'
LABEL org.opencontainers.image.url='https://infosecstreams.com'
LABEL org.opencontainers.image.vendor='InfoSec Streams'
