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
    # The real poll→download→split→transcribe→save loop. The Go binary itself
    # starts and supervises whisper-server (contract §6).
    exec /usr/local/bin/worker "$@"
    ;;
  *)
    echo "unknown mode: ${mode} (expected 'bench' or 'worker')"
    exit 2
    ;;
esac
