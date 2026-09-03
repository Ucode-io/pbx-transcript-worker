package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// LLM cleanup of the raw recognition, between whisper and the DB (contract §7).
// rubaiSTT writes Russian speech in Latin letters ("privet, u tebya yest plani")
// and mangles technical terms ("visper"), which no whisper flag fixes. Gemini
// rewrites the text line by line; timecodes stay ours.
//
// The call audio goes with the lines. Without it the model can only make the
// text tidier, not truer: prod showed "Adashdingiz" (wrong number) recognized
// as "Rossiya televizor" and then polished into confident nonsense. With the
// audio attached the same request corrects against what was actually said,
// while whisper keeps doing the timing and stays the offline fallback.
//
// The cleanup is mandatory: a call whose text could not be polished is not
// saved at all. An unpolished transcript in the column would look identical to
// a polished one and would never be re-queued — which is exactly how the first
// rollout ended up storing raw whisper output for every call. Dropping the call
// instead leaves the row in the queue (§6.5), so the next cycle retries it.

// geminiEndpoint is a var so tests can point it at an httptest server.
var geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

// polishSweeps is how many times we walk the whole key×model rotation. One
// sweep already tries every daily bucket, so a second one only helps against a
// transient 5xx.
const polishSweeps = 2

const polishRetryDelay = 20 * time.Second

// polishAudioMIME/maxPolishAudio: the mixed call, mono 16 kHz mp3 at 32 kbps
// (~4 KB/s), inlined as base64. Gemini bills audio at ~32 tokens/second, so a
// 20-minute call is ~38k tokens and ~6 MB of base64 — inside the 20 MB request
// cap. Longer calls go through without audio rather than failing.
//
// ponytail: the same audio is attached to every chunk of a long call. Median is
// one chunk per call in prod (5.5 lines), so slicing the audio per chunk buys
// nothing until calls get much longer.
const polishAudioMIME = "audio/mp3"
const maxPolishAudio = 12 << 20

// polishChunk caps how many lines go into one request. The answer must come
// back with exactly as many elements as it got, and a whole-call array (a
// 10-minute call is 100-200 whisper segments) drifts by a line or two often
// enough that the all-or-nothing check threw away every correction. Per chunk,
// one bad answer costs one chunk.
const polishChunk = 40

const polishPrompt = `You proofread raw speech-to-text of phone calls at a CRM software company (ucode). Callers speak Uzbek mixed with Russian; the recognizer writes Russian speech in Latin letters and mangles technical terms.

You get the recording of the call (both speakers mixed into one track) and a JSON array of consecutive lines of that call in chronological order: {"speaker": "operator" or "client", "from_sec": 12.8, "to_sec": 15.2, "text": "..."}. The lines are a rough machine transcription of that recording, cut into short fragments, often mid-sentence.

The audio is the truth and the text is only a hint: listen to it and write what is actually said. The recognizer mishears whole words ("Adashdingiz" -> "Rossiya televizor"), so do not trust a line just because it reads like a sentence.

Each line is anchored: from_sec..to_sec is the interval of the recording it covers, and "speaker" is who talks there. Correct a line ONLY from what that speaker says inside its own interval. Never move words from one line into another, never pull in what is said before or after, never merge or split lines. A line whose interval holds no intelligible speech comes back unchanged.

Return a JSON array of strings of the SAME length and in the SAME order: element i is the corrected text of input line i.

Rules:
- Fix recognition errors against the audio, plus spelling and punctuation.
- Ringback tones, beeps, silence and background noise are not speech: if a line is only that, return it unchanged rather than captioning the sound.
- Write Russian speech in Cyrillic — the recognizer transliterates it: "privet, u tebya yest plani na vecher" -> "привет, у тебя есть планы на вечер". Keep Uzbek in Latin script as it is.
- Restore mangled technical and product terms: visper -> Whisper, mpc -> MCP, si-ar-em -> CRM, ERP, API, ucode.
- Never translate, summarize, merge, split, reorder, add or drop lines.
- Keep each line roughly as long as it was: it covers a few seconds of audio, not the whole call.
- Return an unintelligible or empty line unchanged.
- Output the JSON array only.`

// polishLine is one text of the transcript: the slot to write the correction
// back into, plus the context the model needs to make one.
//
// From/To are what keeps the model honest once it can hear the call. Given only
// speaker+text it re-transcribes the whole conversation and redistributes it
// across the slots — prod test put the operator's pitch into a client line.
// Anchored to an interval, each line can only be corrected against its own
// piece of audio.
type polishLine struct {
	slot    *string
	Speaker string  `json:"speaker"`
	From    float64 `json:"from_sec"`
	To      float64 `json:"to_sec"`
	Text    string  `json:"text"`
}

// errPolish marks a failed cleanup, so the batch loop can tell "this one call
// is bad" from "the LLM is unreachable and every call will fail".
var errPolish = errors.New("polish")

// polishTracks rewrites every text of the transcript through the LLM, in place.
// audioPath is the mixed call as mp3; an empty path (or one too big to inline)
// still polishes, just text-only.
// A non-nil error means the transcript must not be stored.
func polishTracks(ctx context.Context, cfg Config, tracks []Track, audioPath string) error {
	lines := transcriptLines(tracks)
	if len(lines) == 0 {
		return nil
	}

	audio := readPolishAudio(audioPath)
	fix := func(chunk []polishLine) ([]string, error) { return geminiFix(ctx, cfg, chunk, audio) }
	changed := 0
	for start := 0; start < len(lines); start += polishChunk {
		n, err := polishChunkLines(fix, lines[start:min(start+polishChunk, len(lines))])
		changed += n
		if err != nil {
			// Both verbs are %w: the caller tells a dead API (stop the batch)
			// from this one call running out of its own budget (skip it).
			return fmt.Errorf("%w at line %d of %d: %w", errPolish, start, len(lines), err)
		}
	}
	log.Printf("polish: %d of %d lines rewritten (audio %d KB)", changed, len(lines), len(audio)/1024)
	rebuildTrackText(tracks)
	return nil
}

// readPolishAudio loads the call audio, or returns nil if there is none or it
// is too big to inline. Missing audio is not an error: text-only polish is what
// this step did before, and losing it must not cost the transcript.
func readPolishAudio(path string) []byte {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("polish: no audio (%v), correcting text only", err)
		return nil
	}
	if info.Size() > maxPolishAudio {
		log.Printf("polish: audio %d MB over the inline cap, correcting text only", info.Size()>>20)
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("polish: no audio (%v), correcting text only", err)
		return nil
	}
	return data
}

// polishChunkLines corrects one chunk and reports how many lines it changed.
// The answer must have exactly as many elements as the chunk had lines, or the
// timecodes would no longer belong to their text; a chunk that comes back
// mis-counted is halved and retried, and a single line cannot be mis-counted.
//
// ponytail: halving costs extra requests only on the rare mangled chunk. If the
// free-tier quota starts to hurt, cap the recursion and fail the call instead.
func polishChunkLines(fix func([]polishLine) ([]string, error), chunk []polishLine) (int, error) {
	fixed, err := fix(chunk)
	if err != nil {
		return 0, err
	}
	if len(fixed) != len(chunk) {
		if len(chunk) == 1 {
			return 0, fmt.Errorf("model returned %d lines for 1: %q", len(fixed), fixed)
		}
		half := len(chunk) / 2
		a, err := polishChunkLines(fix, chunk[:half])
		if err != nil {
			return a, err
		}
		b, err := polishChunkLines(fix, chunk[half:])
		return a + b, err
	}

	changed := 0
	for i, l := range chunk {
		if text := strings.TrimSpace(fixed[i]); text != "" && text != *l.slot {
			*l.slot = text
			changed++
		}
	}
	return changed, nil
}

// transcriptLines lists every text of the transcript in call order — the two
// channels interleaved back into a dialogue, which is the only context the
// model has for a 90-character fragment. A track without segments (mono
// fallback) contributes its whole text as one line.
func transcriptLines(tracks []Track) []polishLine {
	var lines []polishLine
	for i := range tracks {
		if len(tracks[i].Segments) == 0 {
			if tracks[i].Text != "" {
				lines = append(lines, polishLine{
					slot: &tracks[i].Text, Speaker: tracks[i].Speaker, Text: tracks[i].Text,
				})
			}
			continue
		}
		for j := range tracks[i].Segments {
			seg := &tracks[i].Segments[j]
			lines = append(lines, polishLine{
				slot: &seg.Text, Speaker: tracks[i].Speaker, Text: seg.Text,
				From: float64(seg.From) / 1000, To: float64(seg.To) / 1000,
			})
		}
	}
	slices.SortStableFunc(lines, func(a, b polishLine) int { return int(a.From*1000 - b.From*1000) })
	return lines
}

// rebuildTrackText keeps Track.Text — the search field of the whole feature —
// the concatenation of its (now polished) segments.
func rebuildTrackText(tracks []Track) {
	for i := range tracks {
		if len(tracks[i].Segments) == 0 {
			continue
		}
		parts := make([]string, 0, len(tracks[i].Segments))
		for _, seg := range tracks[i].Segments {
			if seg.Text != "" {
				parts = append(parts, seg.Text)
			}
		}
		tracks[i].Text = strings.Join(parts, " ")
	}
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
				// Reasoning parts carry text too — they must not end up in
				// the JSON we parse.
				Thought bool `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func geminiFix(ctx context.Context, cfg Config, lines []polishLine, audio []byte) ([]string, error) {
	input, err := json.Marshal(lines)
	if err != nil {
		return nil, err
	}
	parts := []any{}
	if len(audio) > 0 {
		parts = append(parts, map[string]any{"inline_data": map[string]string{
			"mime_type": polishAudioMIME,
			"data":      base64.StdEncoding.EncodeToString(audio),
		}})
	}
	parts = append(parts, map[string]string{"text": string(input)})

	body, err := json.Marshal(map[string]any{
		"system_instruction": map[string]any{
			"parts": []any{map[string]string{"text": polishPrompt}},
		},
		"contents": []any{map[string]any{"parts": parts}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
			"responseSchema": map[string]any{
				"type":  "ARRAY",
				"items": map[string]any{"type": "STRING"},
			},
			// thinkingLevel left at the model default: transliterated Russian in
			// a 90-character fragment is exactly the case where "low" answered
			// with the input unchanged.
		},
	})
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	pairs := geminiPairs(cfg)
	var lastErr error
	for sweep := 0; sweep < polishSweeps; sweep++ {
		if sweep > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(polishRetryDelay):
			}
		}
		for _, p := range pairs {
			fixed, err := geminiCall(ctx, client, p, body)
			if err == nil {
				return fixed, nil
			}
			if ctx.Err() != nil {
				return nil, err
			}
			lastErr = err
			// Every failure moves to the next bucket: 429 means this key+model
			// is out of quota for the day, 4xx means this key or model is
			// misconfigured, 5xx means this backend is unwell. Another pair may
			// well work, and there is nothing to lose by asking it.
		}
	}
	return nil, lastErr
}

// geminiPair is one API key with one model — one daily free-tier bucket.
type geminiPair struct {
	key, model string
	n          int // position in the rotation, for logs
}

// geminiTurn spreads calls across the buckets instead of draining the first
// one and eating a 429 on every call after that.
var geminiTurn atomic.Uint64

func geminiPairs(cfg Config) []geminiPair {
	var pairs []geminiPair
	for _, k := range cfg.GeminiAPIKeys {
		for _, m := range cfg.GeminiModels {
			pairs = append(pairs, geminiPair{key: k, model: m, n: len(pairs)})
		}
	}
	if len(pairs) < 2 {
		return pairs
	}
	start := int(geminiTurn.Add(1)-1) % len(pairs)
	return append(pairs[start:], pairs[:start]...)
}

func geminiCall(ctx context.Context, client *http.Client, p geminiPair, body []byte) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf(geminiEndpoint, p.model), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", p.key)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()

	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("key %d/%s: status %d: %s", p.n, p.model, resp.StatusCode, truncate(raw, 200))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key %d/%s: status %d: %s", p.n, p.model, resp.StatusCode, parsed.Error.Message)
	}

	var out strings.Builder
	finish := ""
	for _, c := range parsed.Candidates {
		finish = c.FinishReason
		for _, part := range c.Content.Parts {
			if !part.Thought {
				out.WriteString(part.Text)
			}
		}
	}
	var fixed []string
	if err := json.Unmarshal([]byte(out.String()), &fixed); err != nil {
		// Truncated output (MAX_TOKENS) is not malformed JSON by accident —
		// name it, or the log reads as a model that cannot count.
		return nil, fmt.Errorf("key %d/%s: decode output (finish %q): %w", p.n, p.model, finish, err)
	}
	return fixed, nil
}
