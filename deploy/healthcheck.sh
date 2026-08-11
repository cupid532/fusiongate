#!/bin/sh
set -eu

wget -q -T 5 -O- http://127.0.0.1:8787/readyz >/dev/null
if [ -n "${FUSIONGATE_QUALITY_DETECTOR_URL:-}" ]; then
  detector_url=${FUSIONGATE_QUALITY_DETECTOR_URL%/}
  wget -q -T 5 -O- "$detector_url/api/detector/status" >/dev/null
fi
