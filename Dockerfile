# Stage 1: Build PocketBase from source (fork with WebAuthn support)
FROM golang:1.27-alpine AS builder

ARG VERSION=dev

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/pocketbase/pocketbase.Version=${VERSION}" \
    -o /pocketbase \
    ./examples/base

# Stage 2: Download Litestream
FROM alpine:3 AS litestream

ARG LITESTREAM_VERSION=0.3.13
ARG TARGETARCH

RUN wget -q "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-v${LITESTREAM_VERSION}-linux-${TARGETARCH:-amd64}.tar.gz" \
    -O /tmp/litestream.tar.gz \
    && tar -xzf /tmp/litestream.tar.gz -C /usr/local/bin \
    && chmod +x /usr/local/bin/litestream

# Stage 3: Runtime
FROM alpine:3

# flock (util-linux) confirms the outgoing replica has closed both databases
# before this one opens them — see acquire_single_writer in entrypoint.sh.
# Busybox ships a flock applet too; the util-linux package is the deterministic
# source of `-w SECS` on a file descriptor.
RUN apk update && apk add --no-cache ca-certificates tzdata wget flock && rm -rf /var/cache/apk/*

COPY --from=builder /pocketbase /usr/local/bin/pocketbase
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

COPY litestream.yml /etc/litestream.yml

EXPOSE 8090

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
