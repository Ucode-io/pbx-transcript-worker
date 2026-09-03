package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func sample() []Track {
	return []Track{
		{Speaker: "operator", Text: "privet visper", Segments: []Segment{
			{From: 0, To: 1000, Text: "privet"},
			{From: 2000, To: 3000, Text: "visper"},
		}},
		{Speaker: "client", Text: "yaxshi", Segments: []Segment{
			{From: 1000, To: 2000, Text: "yaxshi"},
		}},
	}
}

func TestTranscriptLinesInterleavesChannelsAndWritesBack(t *testing.T) {
	tracks := sample()
	lines := transcriptLines(tracks)

	got := make([]string, len(lines))
	for i, l := range lines {
		got[i] = l.Speaker + ":" + l.Text
	}
	want := []string{"operator:privet", "client:yaxshi", "operator:visper"}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}

	// The slot pointer is the only mapping back — timecodes must survive it.
	*lines[0].slot = "привет"
	*lines[2].slot = "Whisper"
	rebuildTrackText(tracks)
	if tracks[0].Text != "привет Whisper" {
		t.Fatalf("track text not rebuilt: %q", tracks[0].Text)
	}
	if tracks[0].Segments[1].From != 2000 || tracks[0].Segments[1].To != 3000 {
		t.Fatalf("timecodes changed: %+v", tracks[0].Segments[1])
	}
}

// A model that drops a line must not cost the whole chunk: the chunk is halved
// until every line comes back counted.
func TestPolishChunkLinesHalvesOnMiscount(t *testing.T) {
	tracks := []Track{{Speaker: "operator", Segments: []Segment{
		{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"},
	}}}
	chunk := transcriptLines(tracks)

	calls := 0
	fix := func(c []polishLine) ([]string, error) {
		calls++
		out := make([]string, 0, len(c))
		for _, l := range c {
			out = append(out, l.Text+"!")
		}
		if len(c) > 2 {
			return out[1:], nil // drops a line while the chunk is big
		}
		return out, nil
	}

	changed, err := polishChunkLines(fix, chunk)
	if err != nil {
		t.Fatalf("polishChunkLines: %v", err)
	}
	if changed != 4 || calls != 3 {
		t.Fatalf("changed %d in %d calls, want 4 in 3", changed, calls)
	}
	for i, want := range []string{"a!", "b!", "c!", "d!"} {
		if tracks[0].Segments[i].Text != want {
			t.Fatalf("segments = %+v", tracks[0].Segments)
		}
	}
}

// A line the model keeps mis-counting fails the call — the transcript is never
// stored half-polished.
func TestPolishChunkLinesFailsOnHopelessLine(t *testing.T) {
	chunk := transcriptLines([]Track{{Segments: []Segment{{Text: "a"}, {Text: "b"}}}})
	_, err := polishChunkLines(func([]polishLine) ([]string, error) { return nil, nil }, chunk)
	if err == nil {
		t.Fatal("want an error when the model never returns the right count")
	}
}

// A call that ran out of its own budget must stay distinguishable from a dead
// API — processBatch skips the first and stops the batch on the second, and it
// tells them apart through this error chain.
func TestPolishTracksKeepsDeadlineInErrorChain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()

	err := polishTracks(ctx, Config{GeminiAPIKeys: []string{"x"}, GeminiModels: []string{"m"}},
		[]Track{{Speaker: "operator", Segments: []Segment{{Text: "alo"}}}}, "")

	if !errors.Is(err, errPolish) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want both errPolish and DeadlineExceeded in the chain", err)
	}
}

// TestPolishLive is the diagnosis: it calls the real API with the deployed key
// and prints what comes back. GOOGLE_AI_API_KEY=… go test ./cmd/worker -run Live -v
func TestPolishLive(t *testing.T) {
	key := os.Getenv("GOOGLE_AI_API_KEY")
	if key == "" {
		t.Skip("GOOGLE_AI_API_KEY unset")
	}
	cfg := Config{GeminiAPIKeys: csv(key), GeminiModels: csv(env("GEMINI_MODEL", "gemini-3.6-flash"))}
	lines := transcriptLines([]Track{
		{Speaker: "operator", Segments: []Segment{
			{Text: "alo assalomu alaykum"},
			{Text: "privet ya hotel uznat pro vashu si-ar-em"},
		}},
	})

	fixed, err := geminiFix(context.Background(), cfg, lines, readPolishAudio(os.Getenv("POLISH_AUDIO")))
	if err != nil {
		t.Fatalf("geminiFix: %v", err)
	}
	if len(fixed) != len(lines) {
		t.Fatalf("got %d lines for %d: %q", len(fixed), len(lines), fixed)
	}
	for i, f := range fixed {
		t.Logf("%q -> %q", lines[i].Text, f)
	}
}

// A key that is out of its daily quota must cost one request, not the call:
// the next key×model pair in the rotation answers instead. The audio, when
// there is any, rides in the same request.
func TestGeminiFixRotatesPastAnExhaustedKey(t *testing.T) {
	var mu sync.Mutex
	var seenKeys []string
	sawAudio := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Contents []struct {
				Parts []struct {
					InlineData struct {
						MIMEType string `json:"mime_type"`
					} `json:"inline_data"`
				} `json:"parts"`
			} `json:"contents"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		defer mu.Unlock()
		key := r.Header.Get("x-goog-api-key")
		seenKeys = append(seenKeys, key)
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				sawAudio = sawAudio || p.InlineData.MIMEType == polishAudioMIME
			}
		}
		if key == "spent" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"quota"}}`))
			return
		}
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"[\"алло\"]"}]}}]}`))
	}))
	defer srv.Close()

	saved := geminiEndpoint
	geminiEndpoint = srv.URL + "/%s"
	defer func() { geminiEndpoint = saved }()
	geminiTurn.Store(0) // start the rotation at the exhausted key

	cfg := Config{GeminiAPIKeys: []string{"spent", "fresh"}, GeminiModels: []string{"m"}}
	lines := transcriptLines([]Track{{Speaker: "client", Segments: []Segment{{Text: "alo"}}}})

	fixed, err := geminiFix(context.Background(), cfg, lines, []byte("fake mp3"))
	if err != nil {
		t.Fatalf("geminiFix: %v", err)
	}
	if len(fixed) != 1 || fixed[0] != "алло" {
		t.Fatalf("fixed = %q", fixed)
	}
	if len(seenKeys) != 2 || seenKeys[0] != "spent" || seenKeys[1] != "fresh" {
		t.Fatalf("keys tried = %v, want [spent fresh]", seenKeys)
	}
	if !sawAudio {
		t.Fatal("audio was not attached to the request")
	}
}
