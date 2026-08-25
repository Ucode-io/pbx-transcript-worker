package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// LLM cleanup of the raw recognition, between whisper and the DB (contract §7).
// rubaiSTT writes Russian speech in Latin letters ("privet, u tebya yest plani")
// and mangles technical terms ("visper"), which no whisper flag fixes. Gemini
// rewrites the text line by line; timecodes stay ours.
//
// Best-effort by design: on any failure the raw tracks are stored unchanged —
// a rough transcript beats no transcript, and the row is never re-queued.

const geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

const polishAttempts = 3

// polishRetryDelay covers the Google AI Studio free tier, whose quota is
// per-minute (a few requests). One call = one request, so this rarely trips.
const polishRetryDelay = 20 * time.Second

const polishPrompt = `You clean up raw speech-to-text output of phone calls at a CRM software company. Callers speak Uzbek mixed with Russian.

Input is a JSON array of transcript lines from one call, grouped by speaker (all operator lines first, then the client) — not interleaved dialogue.

Return a JSON array of strings of the SAME length and in the SAME order, each line corrected.

Rules:
- Fix recognition errors, spelling and punctuation.
- Restore mangled technical and product terms: visper -> Whisper, mpc -> MCP, si-ar-em -> CRM, ERP, API, ucode.
- Keep Uzbek in Latin script as it is. Write Russian speech in Cyrillic — the recognizer transliterates it: "privet, u tebya yest plani na vecher" -> "привет, у тебя есть планы на вечер".
- Never translate, summarize, merge, split, reorder, add or drop lines.
- Return an unintelligible or empty line unchanged.
- Output the JSON array only.`

// polishTracks rewrites every text of the transcript through the LLM. Returns
// the tracks unchanged when the key is unset or anything goes wrong.
func polishTracks(ctx context.Context, cfg Config, tracks []Track) []Track {
	if cfg.GeminiAPIKey == "" {
		return tracks
	}
	slots := textSlots(tracks)
	lines := make([]string, len(slots))
	for i, s := range slots {
		lines[i] = *s
	}
	if len(lines) == 0 {
		return tracks
	}

	fixed, err := geminiFix(ctx, cfg, lines)
	if err != nil {
		log.Printf("polish skipped: %v", err)
		return tracks
	}
	return applyPolished(tracks, fixed)
}

// textSlots lists every text of the transcript in a fixed order, as pointers so
// the model's answer maps back by index. A track without segments (mono
// fallback) contributes its whole text as one line.
func textSlots(tracks []Track) []*string {
	var slots []*string
	for i := range tracks {
		if len(tracks[i].Segments) == 0 {
			slots = append(slots, &tracks[i].Text)
			continue
		}
		for j := range tracks[i].Segments {
			slots = append(slots, &tracks[i].Segments[j].Text)
		}
	}
	return slots
}

// applyPolished writes the corrected lines back, all or nothing: a length
// mismatch means the model dropped or invented lines, and the timecodes would
// no longer belong to their text.
func applyPolished(tracks []Track, fixed []string) []Track {
	slots := textSlots(tracks)
	if len(fixed) != len(slots) {
		log.Printf("polish skipped: model returned %d lines for %d", len(fixed), len(slots))
		return tracks
	}
	for i, slot := range slots {
		if text := strings.TrimSpace(fixed[i]); text != "" {
			*slot = text
		}
	}
	// Track.Text is the search field of the whole feature — keep it the
	// concatenation of the polished segments.
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
	return tracks
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
	} `json:"candidates"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func geminiFix(ctx context.Context, cfg Config, lines []string) ([]string, error) {
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
			// Proofreading needs no deliberation; thinking only costs latency
			// and free-tier quota.
			"thinkingConfig": map[string]any{"thinkingLevel": "low"},
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
		for _, c := range parsed.Candidates {
			for _, p := range c.Content.Parts {
				if !p.Thought {
					out.WriteString(p.Text)
				}
			}
		}
		var fixed []string
		if err := json.Unmarshal([]byte(out.String()), &fixed); err != nil {
			return nil, fmt.Errorf("decode gemini output: %w", err)
		}
		return fixed, nil
	}
	return nil, lastErr
}
