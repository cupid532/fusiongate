#!/bin/sh
set -eu

addr=${FUSIONGATE_ADDR:-127.0.0.1:8787}
port=${addr##*:}
host=$(hostname -i)
host=${host%% *}
wget -q -T 5 -O- "http://${host}:${port}/readyz" >/dev/null
# Probe FusionGate's own readiness endpoint from inside the container: the app
# may be bound to 0.0.0.0, so connect to the container's primary address from
# `hostname -i` on the configured port. Nothing else runs in this namespace.
