FROM golang:1.25-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/fusiongate ./cmd/fusiongate

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 fusiongate \
    && adduser -S -D -H -u 10001 -G fusiongate fusiongate \
    && mkdir -p /data \
    && chown fusiongate:fusiongate /data
USER fusiongate
WORKDIR /app
COPY --from=build /out/fusiongate /usr/local/bin/fusiongate
ENV FUSIONGATE_ADDR=0.0.0.0:8787 FUSIONGATE_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8787
HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8787/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/fusiongate"]
