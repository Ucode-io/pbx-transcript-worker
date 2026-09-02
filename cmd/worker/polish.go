package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// LLM cleanup of the raw recognition, between whisper and the DB (contract §7).
// rubaiSTT writes Russian speech in Latin letters ("privet, u tebya yest plani")
// and mangles technical terms ("visper"), which no whisper flag fixes. Gemini
// rewrites the text line by line; timecodes stay ours.
//
// The cleanup is mandatory: a call whose text could not be polished is not
// saved at all. An unpolished transcript in the column would look identical to
// a polished one and would never be re-queued — which is exactly how the first
// rollout ended up storing raw whisper output for every call. Dropping the call
// instead leaves the row in the queue (§6.5), so the next cycle retries it.

const geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

const polishAttempts = 3

// polishRetryDelay covers the Google AI Studio free tier, whose quota is
// per-minute (a few requests).
const polishRetryDelay = 20 * time.Second

// polishChunk caps how many lines go into one request. The answer must come
// back with exactly as many elements as it got, and a whole-call array (a
// 10-minute call is 100-200 whisper segments) drifts by a line or two often
// enough that the all-or-nothing check threw away every correction. Per chunk,
// one bad answer costs one chunk.
const polishChunk = 40

const polishPrompt = `You proofread raw speech-to-text of phone calls at a CRM software company (ucode). Callers speak Uzbek mixed with Russian; the recognizer writes Russian speech in Latin letters and mangles technical terms.

Input is a JSON array of consecutive lines of one call in chronological order: {"speaker": "operator" or "client", "text": "..."}. The lines are short fragments, often cut mid-sentence — read the neighbouring lines for context, but correct every line on its own.

Return a JSON array of strings of the SAME length and in the SAME order: element i is the corrected text of input line i.

Rules:
- Fix recognition errors, spelling and punctuation.
- Write Russian speech in Cyrillic — the recognizer transliterates it: "privet, u tebya yest plani na vecher" -> "привет, у тебя есть планы на вечер". Keep Uzbek in Latin script as it is.
- Restore mangled technical and product terms: visper -> Whisper, mpc -> MCP, si-ar-em -> CRM, ERP, API, ucode.
- Never translate, summarize, merge, split, reorder, add or drop lines.
- Return an unintelligible or empty line unchanged.
- Output the JSON array only.`

// polishLine is one text of the transcript: the slot to write the correction
// back into, plus the context the model needs to make one.
type polishLine struct {
	slot    *string
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	from    int64
}

// errPolish marks a failed cleanup, so the batch loop can tell "this one call
// is bad" from "the LLM is unreachable and every call will fail".
var errPolish = errors.New("polish")

// polishTracks rewrites every text of the transcript through the LLM, in place.
// A non-nil error means the transcript must not be stored.
func polishTracks(ctx context.Context, cfg Config, tracks []Track) error {
	lines := transcriptLines(tracks)
	if len(lines) == 0 {
		return nil
	}

	fix := func(chunk []polishLine) ([]string, error) { return geminiFix(ctx, cfg, chunk) }
	changed := 0
	for start := 0; start < len(lines); start += polishChunk {
		n, err := polishChunkLines(fix, lines[start:min(start+polishChunk, len(lines))])
		changed += n
		if err != nil {
			return fmt.Errorf("%w at line %d of %d: %v", errPolish, start, len(lines), err)
		}
	}
	log.Printf("polish: %d of %d lines rewritten", changed, len(lines))
	rebuildTrackText(tracks)
	return nil
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
				slot: &seg.Text, Speaker: tracks[i].Speaker, Text: seg.Text, from: seg.From,
			})
		}
	}
	slices.SortStableFunc(lines, func(a, b polishLine) int { return int(a.from - b.from) })
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

func geminiFix(ctx context.Context, cfg Config, lines []polishLine) ([]string, error) {
	input, err := json.Marshal(lines)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{
		"system_instruction": map[string]any{
			"parts": []any{map[string]string{"text": polishPrompt}},
		},
		"contents": []any{map[string]any{
			"parts": []any{map[string]string{"text": string(input)}},
		}},
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

	url := fmt.Sprintf(geminiEndpoint, cfg.GeminiModel)
	client := &http.Client{Timeout: 2 * time.Minute}
	var lastErr error
	for attempt := 0; attempt < polishAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(polishRetryDelay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", cfg.GeminiAPIKey)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()

		var parsed geminiResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("gemini status %d: %s", resp.StatusCode, truncate(raw, 200))
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("gemini status %d: %s", resp.StatusCode, parsed.Error.Message)
			// Quota (429) and outages (5xx) pass; a bad key or bad request
			// won't get better on retry.
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}

		var out strings.Builder
		finish := ""
		for _, c := range parsed.Candidates {
			finish = c.FinishReason
			for _, p := range c.Content.Parts {
				if !p.Thought {
					out.WriteString(p.Text)
				}
			}
		}
		var fixed []string
		if err := json.Unmarshal([]byte(out.String()), &fixed); err != nil {
			// Truncated output (MAX_TOKENS) is not malformed JSON by accident —
			// name it, or the log reads as a model that cannot count.
			return nil, fmt.Errorf("decode gemini output (finish %q): %w", finish, err)
		}
		return fixed, nil
	}
	return nil, lastErr
}
