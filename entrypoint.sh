#!/usr/bin/env bash
# Dispatches the image's two modes. Default (CMD) is "worker"; the benchmark
# Job overrides args with ["bench"].
set -euo pipefail

mode="${1:-worker}"; shift || true

case "${mode}" in
  bench)
    exec /usr/src/app/bench.sh "$@"
    ;;
  worker)
    # The real poll→download→split→transcribe→save loop lands here after the
    # benchmark confirms CPU is viable (contract §6).
    echo "worker mode not implemented yet — build pending benchmark go/no-go"
    exit 1
    ;;
  *)
    echo "unknown mode: ${mode} (expected 'bench' or 'worker')"
    exit 2
    ;;
esac
