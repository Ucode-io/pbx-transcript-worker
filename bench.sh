#!/usr/bin/env bash
#
# rubaiSTT CPU benchmark. Mirrors the real worker's audio path (contract §6.2/§6.3):
#   split stereo -> 16kHz mono per channel -> whisper-server /inference per channel.
# Starts the server once, warms it (model load not counted), then times both
# channels like a real 2-party call. Prints per-channel + total time and the
# realtime factor. The Apple-Silicon/Metal reference from the contract is
# ~53s for a 2:01 call = ~0.44x realtime.
set -euo pipefail

MODEL="${WHISPER_MODEL:?WHISPER_MODEL not set}"
VAD="${VAD_MODEL:?VAD_MODEL not set}"
PORT="${WHISPER_PORT:-8137}"
THREADS="${THREADS:-$(nproc)}"
INPUT="${1:-/app/samples/sample-stereo-2min.wav}"

echo "=========================================="
echo " rubaiSTT CPU benchmark"
echo "=========================================="
echo "host CPUs available : $(nproc)"
echo "threads (-t)        : ${THREADS}"
echo "input               : ${INPUT}"

work="$(mktemp -d)"
SRV_PID=""
cleanup() {
  [ -n "${SRV_PID}" ] && kill "${SRV_PID}" 2>/dev/null || true
  rm -rf "${work}"
}
trap cleanup EXIT

dur="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "${INPUT}" 2>/dev/null || echo '')"
echo "audio duration      : ${dur:-unknown}s per channel"
echo

# --- split channels: Left = operator, Right = client (contract §6.2) ---------
echo "[1/4] splitting channels + resampling to 16kHz mono..."
ffmpeg -hide_banner -loglevel error -i "${INPUT}" \
  -filter_complex "channelsplit=channel_layout=stereo[l][r]" \
  -map "[l]" -ar 16000 -ac 1 "${work}/operator.wav" \
  -map "[r]" -ar 16000 -ac 1 "${work}/client.wav"

# --- start whisper-server with the exact mandatory flags (contract §6.3) ------
echo "[2/4] starting whisper-server (model stays resident)..."
whisper-server -m "${MODEL}" -l uz \
  --vad -vm "${VAD}" -sns -ml 90 -sow \
  -t "${THREADS}" \
  --host 127.0.0.1 --port "${PORT}" >"${work}/server.log" 2>&1 &
SRV_PID=$!

printf "      waiting for readiness"
ready=0
for _ in $(seq 1 120); do
  if curl -sf "http://127.0.0.1:${PORT}/" >/dev/null 2>&1; then ready=1; echo " ok"; break; fi
  if ! kill -0 "${SRV_PID}" 2>/dev/null; then
    echo " FAILED — server exited"; echo "---- server.log ----"; cat "${work}/server.log"; exit 1
  fi
  printf "."; sleep 1
done
[ "${ready}" -eq 1 ] || { echo " TIMEOUT"; cat "${work}/server.log"; exit 1; }

infer() { # $1 = wav file, $2 = output json
  curl -sf "http://127.0.0.1:${PORT}/inference" \
    -F file=@"$1" -F response_format=json -o "$2"
}

# --- warmup (loads model fully into RAM; not counted) ------------------------
echo "[3/4] warmup pass (not timed)..."
infer "${work}/operator.wav" "${work}/warmup.json" || { echo "warmup failed"; cat "${work}/server.log"; exit 1; }

# --- timed run: both channels, as in a real call ----------------------------
echo "[4/4] timed run (2 channels)..."
t0="$(date +%s.%N)"
infer "${work}/operator.wav" "${work}/operator.json"
t1="$(date +%s.%N)"
infer "${work}/client.wav"   "${work}/client.json"
t2="$(date +%s.%N)"

op="$(echo "${t1} - ${t0}" | bc)"
cl="$(echo "${t2} - ${t1}" | bc)"
tot="$(echo "${t2} - ${t0}" | bc)"

echo
echo "=================== RESULTS ==================="
printf "operator channel : %6.1fs\n" "${op}"
printf "client channel   : %6.1fs\n" "${cl}"
printf "total (2 ch)     : %6.1fs\n" "${tot}"
if [ -n "${dur}" ]; then
  rt="$(echo "scale=2; ${tot} / ${dur}" | bc)"
  echo "realtime factor  : ${rt}x  (total / audio-sec; lower=better; Mac ref ~0.44x)"
fi
echo "=============================================="
echo
echo "--- operator.json (first 400 chars) ---"; head -c 400 "${work}/operator.json"; echo
echo "--- client.json  (first 400 chars) ---";  head -c 400 "${work}/client.json";  echo
