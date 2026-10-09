# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache dependencies first (project is dependency-free, kept for future growth).
COPY go.mod ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dockerfast ./cmd/dockerfast

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -H -u 10001 dockerfast && \
    mkdir -p /data && chown -R dockerfast:dockerfast /data
COPY --from=builder /out/dockerfast /usr/local/bin/dockerfast

USER dockerfast
VOLUME ["/data"]
EXPOSE 5000

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:5000/healthz || exit 1

ENTRYPOINT ["dockerfast"]
CMD ["-config", "/etc/dockerfast/config.json"]
