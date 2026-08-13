#!/bin/sh
set -eu

addr=${FUSIONGATE_ADDR:-127.0.0.1:8787}
port=${addr##*:}
host=$(hostname -i)
host=${host%% *}
wget -q -T 5 -O- "http://${host}:${port}/readyz" >/dev/null
# The detector shares this container's network namespace in the production
# compose layout, but Docker may start FusionGate before the sidecar listener.
# Probe the core service only; the detector has its own container healthcheck.
