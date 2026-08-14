# PBX transcript worker — whisper.cpp v1.9.2 + rubaiSTT (Uzbek-tuned Whisper).
# Built natively on x86 by the central Ucode-io/ci-cd GitHub workflow → ghcr.io.
# ONE image, two modes (see entrypoint.sh):
#   bench   — one-shot CPU benchmark (contract §6.4/§8), for the go/no-go on CPU
#   worker  — the real poll→download→split→transcribe→save loop (contract §6)

############################################################
# Stage 1 — build whisper.cpp for linux/amd64 (CPU only)
############################################################
FROM debian:bookworm-slim AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates cmake make g++ \
    && rm -rf /var/lib/apt/lists/*

ARG WHISPER_TAG=v1.9.2
RUN git clone --depth 1 --branch ${WHISPER_TAG} \
      https://github.com/ggml-org/whisper.cpp /src

WORKDIR /src
# GGML_NATIVE=OFF for portability (built on a CI runner, run on EPYC worker08);
# AVX2/FMA/F16C is the safe Zen baseline for speed. Static libs => self-contained
# binaries. Overridable so a scalar (no-SIMD) build can run under qemu locally.
ARG CPU_FLAGS="-DGGML_AVX=ON -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON"
RUN cmake -B build \
      -DCMAKE_BUILD_TYPE=Release \
      -DBUILD_SHARED_LIBS=OFF \
      -DGGML_NATIVE=OFF \
      ${CPU_FLAGS} \
      -DWHISPER_BUILD_TESTS=OFF \
    && cmake --build build --config Release -j "$(nproc)" \
         --target whisper-server whisper-cli

############################################################
# Stage 2 — download models (cached separately from the compile)
############################################################
FROM debian:bookworm-slim AS models

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /models
# rubaiSTT v2 medium, ggml q8_0 ~823 MB (contract §6.1). Apache-2.0.
RUN curl --fail --location --retry 3 -o ggml-rubaistt.bin \
      https://github.com/MuhammadMirrr/uzbek-dictation/releases/download/v1.0/ggml-rubaistt.bin
# Silero VAD v5.1.2 ~865 KB (whisper.cpp's own VAD model on HuggingFace).
RUN curl --fail --location --retry 3 -o ggml-silero-v5.1.2.bin \
      https://huggingface.co/ggml-org/whisper-vad/resolve/main/ggml-silero-v5.1.2.bin
# Sanity: rubaiSTT must be a few hundred MB, not an HTML error page.
RUN test "$(stat -c%s ggml-rubaistt.bin)" -gt 500000000

############################################################
# Stage 3 — runtime
############################################################
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
      ffmpeg curl ca-certificates libgomp1 bc \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /usr/src/app

COPY --from=build  /src/build/bin/whisper-server  /usr/local/bin/whisper-server
COPY --from=build  /src/build/bin/whisper-cli     /usr/local/bin/whisper-cli
COPY --from=models /models/ggml-rubaistt.bin      /app/models/ggml-rubaistt.bin
COPY --from=models /models/ggml-silero-v5.1.2.bin /app/models/ggml-silero-v5.1.2.bin

# ~2-min stereo sample from whisper.cpp's jfk clip, for the bench mode with no
# external file (timing is representative; text is gibberish — English/Uzbek model).
COPY --from=build /src/samples/jfk.wav /app/samples/jfk.wav
RUN ffmpeg -hide_banner -loglevel error -stream_loop 10 -i /app/samples/jfk.wav \
      -ac 2 -ar 48000 /app/samples/sample-stereo-2min.wav

COPY bench.sh entrypoint.sh /usr/src/app/
RUN chmod +x /usr/src/app/bench.sh /usr/src/app/entrypoint.sh

ENV WHISPER_MODEL=/app/models/ggml-rubaistt.bin \
    VAD_MODEL=/app/models/ggml-silero-v5.1.2.bin \
    WHISPER_PORT=8137 \
    THREADS=4

# Code kept out of /app because the cluster Vault Agent writes /app/.env at
# deploy (same reason KP keeps code in /usr/src/app).
ENTRYPOINT ["/usr/src/app/entrypoint.sh"]
CMD ["worker"]
