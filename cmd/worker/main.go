// Command worker drains PBX calls that need transcription (contract §6):
// poll pbx_list_untranscribed → download the recording → split channels →
// recognize each with a resident whisper-server → write the transcript back
// via pbx_save_transcript. An empty transcript column is the whole queue; the
// worker keeps no state of its own.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ws, err := startWhisperServer(ctx, cfg)
	if err != nil {
		log.Fatalf("start whisper-server: %v", err)
	}
	defer ws.stop()

	if err := waitReady(ctx, cfg); err != nil {
		log.Fatalf("whisper-server not ready: %v", err)
	}
	log.Printf("whisper-server ready on %s", cfg.whisperBaseURL())

	run(ctx, cfg, newFaaSClient(cfg.FaaSURL))
	log.Print("worker stopped")
}

func run(ctx context.Context, cfg Config, client *faasClient) {
	log.Printf("worker started: %d app(s), poll every %s, batch %d, %d threads",
		len(cfg.AppIDs), cfg.PollInterval, cfg.BatchLimit, cfg.Threads)
	for {
		for _, appID := range cfg.AppIDs {
			if ctx.Err() != nil {
				return
			}
			processBatch(ctx, cfg, client, appID)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.PollInterval):
		}
	}
}

func processBatch(ctx context.Context, cfg Config, client *faasClient, appID string) {
	calls, err := client.listUntranscribed(ctx, appID, cfg.BatchLimit)
	if err != nil {
		log.Printf("list untranscribed (app %s): %v", short(appID), err)
		return
	}
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		if ctx.Err() != nil {
			return
		}
		if c.CallUUID == "" || c.URL == "" || seen[c.CallUUID] {
			continue
		}
		seen[c.CallUUID] = true
		if err := processCall(ctx, cfg, client, appID, c); err != nil {
			// Log and move on: a bad call must not stall the queue. It stays
			// untranscribed and is retried next cycle (contract §6.5).
			log.Printf("call %s: %v", c.CallUUID, err)
			if errors.Is(err, errPolish) && !errors.Is(err, context.DeadlineExceeded) {
				// The LLM is down, out of quota or misconfigured: the rest of
				// the batch would burn a whisper run each only to fail the same
				// way. Give it until the next poll.
				//
				// A call that ran out of CallTimeout is NOT that case — it is
				// one bad call, and stopping the batch for it lets a single
				// too-long recording block the queue for every other call.
				return
			}
		}
	}
}

func processCall(ctx context.Context, cfg Config, client *faasClient, appID string, call Call) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.CallTimeout)
	defer cancel()

	workDir, err := os.MkdirTemp("", "call-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	transcript, err := transcribeCall(ctx, cfg, call, workDir)
	if err != nil {
		return err
	}
	if err := client.saveTranscript(ctx, appID, call.CallUUID, transcript); err != nil {
		return fmt.Errorf("save transcript: %w", err)
	}
	log.Printf("transcribed call %s (%s)", call.CallUUID, call.Source)
	return nil
}

// whisperServer supervises the resident whisper-server child process. If it
// dies unexpectedly the worker exits so Kubernetes restarts the pod rather than
// spinning while every inference fails.
type whisperServer struct{ cmd *exec.Cmd }

func startWhisperServer(ctx context.Context, cfg Config) (*whisperServer, error) {
	args := []string{
		"-m", cfg.WhisperModel, "-l", cfg.Language,
		"--vad", "-vm", cfg.VADModel, "-sns", "-ml", "90", "-sow",
		"-t", strconv.Itoa(cfg.Threads),
		"--host", cfg.WhisperHost, "--port", strconv.Itoa(cfg.WhisperPort),
	}
	cmd := exec.Command(cfg.WhisperBin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		if ctx.Err() == nil {
			log.Fatalf("whisper-server exited unexpectedly: %v", err)
		}
	}()
	return &whisperServer{cmd: cmd}, nil
}

func (w *whisperServer) stop() {
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
}

func waitReady(ctx context.Context, cfg Config) error {
	deadline := time.Now().Add(2 * time.Minute)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.whisperBaseURL()+"/", nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timed out waiting for readiness")
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return s
}
