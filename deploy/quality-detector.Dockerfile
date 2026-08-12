FROM alpine:3.22 AS source
RUN apk add --no-cache ca-certificates curl tar
ARG DETECTOR_COMMIT=c0035f9695406ca0ebd00899e9c080294f894412
ARG DETECTOR_SHA256=a62e586b4da1a5d026b062119f7ec4b786f14162503a59dac7698864f2a6369d
RUN curl -fsSL "https://github.com/chen-006/gpt56_api_detector/archive/${DETECTOR_COMMIT}.tar.gz" -o /tmp/detector.tar.gz \
    && echo "${DETECTOR_SHA256}  /tmp/detector.tar.gz" | sha256sum -c - \
    && mkdir -p /detector \
    && tar -xzf /tmp/detector.tar.gz --strip-components=1 -C /detector \
    && test "$(cat /detector/VERSION)" = "4.0.1"

FROM node:24-alpine
ARG FUSIONGATE_BUILD_REVISION=unknown
ARG FUSIONGATE_BUILD_SOURCE=https://github.com/cupid532/fusiongate
ARG FUSIONGATE_BUILD_VERSION=dev
LABEL org.opencontainers.image.revision="$FUSIONGATE_BUILD_REVISION" \
      org.opencontainers.image.source="$FUSIONGATE_BUILD_SOURCE" \
      org.opencontainers.image.version="$FUSIONGATE_BUILD_VERSION"
RUN apk add --no-cache python3 ca-certificates \
    && addgroup -S -g 10002 detector \
    && adduser -S -D -H -u 10002 -G detector detector \
    && mkdir -p /runs \
    && chown detector:detector /runs
COPY --from=source --chown=detector:detector /detector /app
USER detector
WORKDIR /app
VOLUME ["/runs"]
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
  CMD wget -qO- http://127.0.0.1:18789/api/health || exit 1
ENTRYPOINT ["python3", "gpt56_vnext_web.py", "--port", "18789", "--no-browser", "--runs-root", "/runs"]
