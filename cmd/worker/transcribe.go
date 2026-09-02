package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Transcript is the JSON stored in pbx_calls.transcript (contract §3.2).
type Transcript struct {
	Version  int     `json:"version"`
	Model    string  `json:"model"`
	Language string  `json:"language"`
	Source   string  `json:"source"` // "stereo" | "mono"
	Tracks   []Track `json:"tracks"`
}

type Track struct {
	Speaker  string    `json:"speaker"` // operator | client | unknown
	Text     string    `json:"text"`
	Segments []Segment `json:"segments"`
}

type Segment struct {
	From int64  `json:"from"` // ms from start of the recording
	To   int64  `json:"to"`
	Text string `json:"text"`
}

// transcribeCall runs one call end to end and returns the transcript JSON.
// Left channel = operator, right = client (contract §6.2). Channels are
// recognized sequentially — the shape the benchmark validated (~1.29× realtime
// per call on worker08). Parallelizing across two whisper-server instances is a
// later, separately-benchmarked optimization.
func transcribeCall(ctx context.Context, cfg Config, call Call, workDir string) (string, error) {
	recPath := filepath.Join(workDir, "recording")
	if err := download(ctx, cfg, call.URL, recPath); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}

	source := call.Source
	if source != "stereo" {
		source = "mono"
	}
	// A "stereo" file that is actually mono (fallback recordings) can't be
	// split — degrade to a single unknown track rather than failing.
	if source == "stereo" && probeChannels(ctx, recPath) < 2 {
		source = "mono"
	}

	var tracks []Track
	if source == "stereo" {
		opWav := filepath.Join(workDir, "operator.wav")
		clWav := filepath.Join(workDir, "client.wav")
		if err := splitStereo(ctx, recPath, opWav, clWav); err != nil {
			return "", fmt.Errorf("split: %w", err)
		}
		for _, ch := range []struct {
			speaker string
			path    string
		}{{"operator", opWav}, {"client", clWav}} {
			text, segs, err := infer(ctx, cfg, ch.path)
			if err != nil {
				return "", fmt.Errorf("inference (%s): %w", ch.speaker, err)
			}
			tracks = append(tracks, Track{Speaker: ch.speaker, Text: text, Segments: segs})
		}
	} else {
		monoWav := filepath.Join(workDir, "mono.wav")
		if err := toMono16k(ctx, recPath, monoWav); err != nil {
			return "", fmt.Errorf("resample: %w", err)
		}
		text, segs, err := infer(ctx, cfg, monoWav)
		if err != nil {
			return "", fmt.Errorf("inference: %w", err)
		}
		tracks = []Track{{Speaker: "unknown", Text: text, Segments: segs}}
	}

	// LLM cleanup before the transcript ever reaches the DB (contract §7):
	// Latin-script Russian to Cyrillic, mangled terms restored. Mandatory — a
	// call we could not polish is left in the queue instead of stored raw.
	if err := polishTracks(ctx, cfg, tracks); err != nil {
		return "", err
	}

	doc := Transcript{
		Version:  1,
		Model:    cfg.ModelName,
		Language: cfg.Language,
		Source:   source,
		Tracks:   tracks,
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// download fetches a recording to dest. The stored URL was host-validated at
// save time, but we re-check the initial host here too (the worker fetches it
// server-side) and cap the size.
func download(ctx context.Context, cfg Config, rawURL, dest string) error {
	rawURL = strings.TrimSpace(rawURL)
	// recording_url is stored as a CDN-relative path (bucket/Media/file);
	// stereo_recording_url is already absolute. Normalize the relative form to
	// the CDN host so both go through the same https + host-allowlist path.
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + cfg.CDNHost + "/" + strings.TrimPrefix(rawURL, "/")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("bad url: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("url is not https")
	}
	if !strings.EqualFold(parsed.Hostname(), cfg.CDNHost) {
		return fmt.Errorf("url host %q not allowed", parsed.Hostname())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/") {
		return fmt.Errorf("unexpected content-type %q (likely an error page)", ct)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, cfg.MaxDownloadBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n > cfg.MaxDownloadBytes {
		return fmt.Errorf("recording exceeds %d bytes", cfg.MaxDownloadBytes)
	}
	if n == 0 {
		return fmt.Errorf("empty recording")
	}
	return nil
}

func probeChannels(ctx context.Context, path string) int {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-select_streams", "a:0", "-show_entries", "stream=channels",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// splitStereo maps L→operator, R→client, each downmixed to 16kHz mono (§6.2).
func splitStereo(ctx context.Context, input, opWav, clWav string) error {
	return runFFmpeg(ctx,
		"-i", input,
		"-filter_complex", "channelsplit=channel_layout=stereo[l][r]",
		"-map", "[l]", "-ar", "16000", "-ac", "1", opWav,
		"-map", "[r]", "-ar", "16000", "-ac", "1", clWav,
	)
}

func toMono16k(ctx context.Context, input, outWav string) error {
	return runFFmpeg(ctx, "-i", input, "-ar", "16000", "-ac", "1", outWav)
}

func runFFmpeg(ctx context.Context, args ...string) error {
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	cmd := exec.CommandContext(ctx, "ffmpeg", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// whisperResponse is whisper-server's verbose_json shape. start/end are seconds.
type whisperResponse struct {
	Text     string `json:"text"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// infer sends one wav to whisper-server /inference (the recognition flags were
// fixed at server start, contract §6.3) and returns text plus timed segments.
func infer(ctx context.Context, cfg Config, wavPath string) (string, []Segment, error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(wavPath))
	if err != nil {
		return "", nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", nil, err
	}
	_ = mw.WriteField("response_format", "verbose_json")
	if err := mw.Close(); err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.whisperBaseURL()+"/inference", &body)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("whisper status %d: %s", resp.StatusCode, truncate(raw, 200))
	}

	var wr whisperResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return "", nil, fmt.Errorf("decode whisper response: %w", err)
	}
	segs := make([]Segment, 0, len(wr.Segments))
	for _, s := range wr.Segments {
		segs = append(segs, Segment{
			From: int64(s.Start * 1000),
			To:   int64(s.End * 1000),
			Text: strings.TrimSpace(s.Text),
		})
	}
	return strings.TrimSpace(wr.Text), segs, nil
}
