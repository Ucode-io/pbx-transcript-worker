# pbx-transcript-worker

Speech-to-text worker for PBX calls (backend contract: `pbx-transcript-backend-contract.md`, §6).
Engine: **whisper.cpp v1.9.2** + **rubaiSTT v2 medium** (Uzbek-tuned Whisper, Apache-2.0).

Ships the ucode standard way: push to `master` → **`Ucode-io/ci-cd`** GitHub workflow
builds the image natively on x86 → **ghcr.io** → deployed via ArgoCD. No local
builds, no manual registry push.

One image, two modes (`entrypoint.sh`):
- **`bench`** — one-shot CPU benchmark, to decide if our GPU-less nodes are fast
  enough (contract §6.4/§8). Runs a baked 2-min stereo sample.
- **`worker`** — the real poll→download→split→transcribe→save loop (contract §6).
  *Not implemented yet — pending the benchmark go/no-go.*

Models (rubaiSTT ~823 MB + silero VAD) are **downloaded during the image build**
from the release URLs in the contract — nothing large lives in git.

---

## Step 1 — CPU benchmark (current)

The whole fleet is CPU-only (verified — no GPU on prod/staging/uz). Only prod
**worker08** (8 CPU / 32 GB) has room; fallback **worker03**.

1. **Build** — push to `master`; GitHub Actions builds and publishes
   `ghcr.io/ucode-io/pbx-transcript-worker:latest`. Watch the run under the repo's
   Actions tab.
2. **Run on worker08:**
   ```sh
   kubectl --kubeconfig=/Users/user/.kube/ucode.conf apply -f k8s/job-bench.yaml
   kubectl --kubeconfig=/Users/user/.kube/ucode.conf -n knative-fn logs -f job/rubaistt-bench
   ```
3. **Read** the `RESULTS` block — per-channel time, total, **realtime factor**
   (`total ÷ audio-sec`). Apple-Silicon/Metal reference: ~0.44×.
   - **≤ ~1×** → CPU keeps up; ship on worker08.
   - **~1–4×** → fine as a background drain at low call volume.
   - **≫ 4×** → CPU too slow; the number justifies a GPU box.

Cleanup: `kubectl -n knative-fn delete job rubaistt-bench` (auto-removes after 1h).

> If the pod shows **ImagePullBackOff**, the ghcr package is private and the
> namespace lacks the pull secret — either make the package public or add the
> secret.

### Benchmark a real call
```sh
docker run --rm -v "$PWD/call.webm:/in.webm" \
  ghcr.io/ucode-io/pbx-transcript-worker:latest bench /in.webm
```
(English-through-Uzbek sample gives representative *timing*; real Uzbek audio
gives real *text*.)

---

## Step 2 — worker loop (built ✅)

CPU is confirmed viable (1.29× realtime/call on worker08), so the `worker` mode
is implemented: a stdlib-only Go binary (`cmd/worker`) that supervises a
resident `whisper-server` and loops:

1. `pbx_list_untranscribed` per app_id → batch of calls (deduped by call_uuid)
2. download the CDN recording → ffmpeg channel-split (or resample if mono)
3. recognize each channel via `whisper-server /inference` (sequential — the
   benchmarked path)
4. proofread every line through Gemini (contract §7) — **with the call audio in
   the same request** (mixed to one 16 kHz mono mp3). Text alone only made the
   recognition tidier, not truer; with the audio the model corrects what was
   misheard: prod line `engine sound. Alo. Rayon, salom … Rossiya televizor` came
   back as `Alo. Vaalaykum assalom … Adashdingiz, og'ayni` (wrong number). It also
   fixes what no whisper flag can — Russian speech written in Latin letters and
   mangled terms (`visper`). Timecodes are ours and never leave. The channels are
   interleaved back into a dialogue and sent in chunks of 40 lines, so the model
   sees context and one bad answer costs one chunk, not the call. Every line
   carries its `from_sec`/`to_sec` and its speaker: hearing the mixed call, the
   model otherwise re-transcribes it and redistributes speech across the slots
   (a test put the operator's pitch into a client line). **Mandatory**:
   a call that could not be polished is not saved — it stays in the queue and is
   retried next cycle, because an unpolished transcript is indistinguishable from
   a polished one once it is in the column and would never be redone
5. assemble the two-track JSON (contract §3.2)
6. `pbx_save_transcript` → write it back

It talks to the FaaS **in-cluster** at
`http://professional-crm-pbx-integration-call.knative-fn.u-code.io` (no auth
token; `app_id` in the body is the identity). Config is all env — see
`cmd/worker/config.go`. `APP_IDS` (comma-separated ProfessionalCRM project keys)
and `GOOGLE_AI_API_KEY` are required; the worker exits at startup without them.

`GOOGLE_AI_API_KEY` and `GEMINI_MODEL` both take comma-separated lists. The free
tier meters requests **per project and per model** (20/day —
`GenerateRequestsPerDayPerProjectPerModel-FreeTier`), so every key×model pair is
its own daily bucket: 3 keys × 1 model = 60 requests/day, and a call costs 1
request per 40 lines (median 1). Calls rotate through the pairs and a 429 moves
on to the next one, so an exhausted key costs one request, not the call.

To redo calls transcribed before the cleanup worked, clear their `transcript`
column — an empty column *is* the queue, so the worker picks them up again.
Watch `polish: N of M lines rewritten` in the logs to confirm it ran.

### Deploy + E2E test
```sh
# 0. prerequisite: the FaaS must expose pbx_save_transcript (deploy that repo)
# 1. push this repo → CI rebuilds ghcr image with the worker binary
# 2. create the project-key secret (keeps it out of git)
kubectl -n ucode-prod create secret generic pbx-transcript-worker-config \
  --from-literal=APP_IDS="<prof-crm-app-id>" \
  --from-literal=GOOGLE_AI_API_KEY="<key1>,<key2>,<key3>"   # list = more daily quota
# 3. roll it out (pinned to worker08)
kubectl --kubeconfig=/Users/user/.kube/ucode.conf apply -f k8s/deployment.yaml
kubectl --kubeconfig=/Users/user/.kube/ucode.conf -n ucode-prod logs -f deploy/pbx-transcript-worker
```

For the standard ucode rollout, add a `deployments/clusters/…/pbx-transcript-worker`
entry (ArgoCD, microservice_v2) instead of `kubectl apply`.
