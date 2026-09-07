package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the worker's whole runtime surface, all from env so the deploy
// (Vault-injected .env) is the single source of truth.
type Config struct {
	// FaaSURL is the in-cluster ksvc of professional-crm-pbx-integration-call.
	// The worker POSTs invoke bodies here directly — same URL the gateway's
	// ExecKnative uses, no auth token (identity is app_id in the body).
	FaaSURL string
	// AppIDs are the ProfessionalCRM project keys to drain, one per env. Each
	// invoke carries an app_id, which the FaaS uses as the object-builder
	// X-API-KEY, so a call is only visible under its own project's app_id.
	AppIDs []string

	WhisperBin   string
	WhisperModel string
	VADModel     string
	WhisperHost  string
	WhisperPort  int
	Threads      int
	Language     string
	ModelName    string // recorded in the transcript JSON "model" field
	// MaxSegmentChars is whisper's -ml: the longest text one segment may hold.
	// It is what decides how finely the channel is cut, and with --vad that is
	// also the timestamp resolution: VAD strips the silence, so a segment's
	// interval is stretched over every pause inside it (a 129-char client line
	// covered 22.4s..79.55s of a 74s call). Coarse segments cannot be
	// interleaved back into a dialogue — the client's "mhm" lands wherever its
	// block starts. Tune here if the dialogue order still reads wrong.
	MaxSegmentChars int

	PollInterval time.Duration
	BatchLimit   int

	// Gemini (Google AI Studio) proofreads the raw recognition before it is
	// stored (§7). Required: the cleanup is part of the transcript, not an
	// optional extra, so the worker refuses to start without a key rather than
	// quietly filling the column with raw whisper output.
	//
	// Both are comma-separated lists. The free tier meters requests per project
	// AND per model (GenerateRequestsPerDayPerProjectPerModel-FreeTier: 20/day),
	// so every key×model pair is its own daily bucket: 3 keys × 2 models = 6.
	GeminiAPIKeys []string
	GeminiModels  []string

	// CDNHost is the only host a recording URL may point at — the worker
	// fetches it server-side, so this is the SSRF allowlist (matches the FaaS).
	CDNHost          string
	MaxDownloadBytes int64
	CallTimeout      time.Duration
}

func loadConfig() (Config, error) {
	c := Config{
		FaaSURL:          env("FAAS_URL", "http://professional-crm-pbx-integration-call.knative-fn.u-code.io"),
		WhisperBin:       env("WHISPER_BIN", "whisper-server"),
		WhisperModel:     env("WHISPER_MODEL", "/app/models/ggml-rubaistt.bin"),
		VADModel:         env("VAD_MODEL", "/app/models/ggml-silero-v5.1.2.bin"),
		WhisperHost:      env("WHISPER_HOST", "127.0.0.1"),
		WhisperPort:      envInt("WHISPER_PORT", 8137),
		Threads:          envInt("THREADS", 4),
		Language:         env("LANGUAGE", "uz"),
		ModelName:        env("MODEL_NAME", "rubaistt_v2_medium"),
		MaxSegmentChars:  envInt("MAX_SEGMENT_CHARS", 30),
		PollInterval:     time.Duration(envInt("POLL_INTERVAL_SECONDS", 30)) * time.Second,
		BatchLimit:       envInt("BATCH_LIMIT", 5),
		GeminiAPIKeys:    csv(env("GOOGLE_AI_API_KEY", "")),
		GeminiModels:     csv(env("GEMINI_MODEL", "gemini-3.6-flash")),
		CDNHost:          env("CDN_HOST", "cdn.u-code.io"),
		MaxDownloadBytes: int64(envInt("MAX_DOWNLOAD_MB", 200)) * 1024 * 1024,
		// 600s used to cut off every stereo call longer than ~4.5 minutes: prod
		// shows 262s of stereo finishing and 271s timing out, i.e. ~1.13s of
		// work per second of mono audio and two channels per call. 1800s covers
		// stereo up to ~13 minutes; longer calls still fail and retry forever,
		// which needs a skip marker in the FaaS, not a bigger number here.
		CallTimeout: time.Duration(envInt("CALL_TIMEOUT_SECONDS", 1800)) * time.Second,
	}

	c.AppIDs = csv(env("APP_IDS", ""))
	if len(c.AppIDs) == 0 {
		return c, fmt.Errorf("APP_IDS is required (comma-separated ProfessionalCRM project app_ids)")
	}
	if len(c.GeminiAPIKeys) == 0 {
		return c, fmt.Errorf("GOOGLE_AI_API_KEY is required (transcripts are stored only after the LLM cleanup)")
	}
	if len(c.GeminiModels) == 0 {
		return c, fmt.Errorf("GEMINI_MODEL must not be empty")
	}
	return c, nil
}

func (c Config) whisperBaseURL() string {
	return fmt.Sprintf("http://%s:%d", c.WhisperHost, c.WhisperPort)
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// csv splits a comma-separated env value, dropping blanks.
func csv(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
