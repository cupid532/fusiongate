FROM node:24-alpine AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.27.0-alpine AS build
ARG FUSIONGATE_BUILD_REVISION=unknown
RUN apk add --no-cache build-base curl
ARG SING_BOX_VERSION=1.13.13
RUN case "$(apk --print-arch)" in \
      x86_64) arch=amd64 ;; \
      aarch64) arch=arm64 ;; \
      *) echo "unsupported Alpine architecture: $(apk --print-arch)" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://github.com/SagerNet/sing-box/releases/download/v${SING_BOX_VERSION}/sing-box-${SING_BOX_VERSION}-linux-${arch}-musl.tar.gz" \
    | tar -xz --strip-components=1 -C /tmp \
    && install -m 0755 /tmp/sing-box /out-sing-box \
    && curl -fsSL "https://raw.githubusercontent.com/SagerNet/sing-box/v${SING_BOX_VERSION}/LICENSE" -o /out-sing-box-LICENSE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN rm -rf internal/fusiongate/ui \
    && mkdir -p internal/fusiongate/ui
COPY --from=web-build /src/web/dist/ internal/fusiongate/ui/
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/fusiongate/fusiongate/internal/fusiongate.BuildRevision=${FUSIONGATE_BUILD_REVISION}" \
    -o /out/fusiongate ./cmd/fusiongate

FROM alpine:3.22
ARG FUSIONGATE_BUILD_REVISION=unknown
ARG FUSIONGATE_BUILD_SOURCE=https://github.com/cupid532/fusiongate
ARG FUSIONGATE_BUILD_VERSION=dev
LABEL org.opencontainers.image.revision="$FUSIONGATE_BUILD_REVISION" \
      org.opencontainers.image.source="$FUSIONGATE_BUILD_SOURCE" \
      org.opencontainers.image.version="$FUSIONGATE_BUILD_VERSION"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 fusiongate \
    && adduser -S -D -H -u 10001 -G fusiongate fusiongate \
    && mkdir -p /data \
    && chown fusiongate:fusiongate /data
USER fusiongate
WORKDIR /app
COPY --from=build /out/fusiongate /usr/local/bin/fusiongate
COPY --from=build /out-sing-box /usr/local/bin/sing-box
COPY --from=build /out-sing-box-LICENSE /usr/share/licenses/sing-box/LICENSE
COPY deploy/healthcheck.sh /usr/local/bin/fusiongate-healthcheck
ENV FUSIONGATE_ADDR=0.0.0.0:8787 FUSIONGATE_DATA_DIR=/data FUSIONGATE_SING_BOX_PATH=/usr/local/bin/sing-box
VOLUME ["/data"]
EXPOSE 8787
HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=5 \
  CMD /usr/local/bin/fusiongate-healthcheck
ENTRYPOINT ["/usr/local/bin/fusiongate"]
