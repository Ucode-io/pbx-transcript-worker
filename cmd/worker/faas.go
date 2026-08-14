package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Call is one row returned by pbx_list_untranscribed (§5).
type Call struct {
	GUID     string `json:"guid"`
	CallUUID string `json:"call_uuid"`
	Source   string `json:"source"` // "stereo" | "mono"
	URL      string `json:"url"`    // preferred recording (stereo wins)
}

// faasClient invokes the professional-crm-pbx-integration-call knative function.
type faasClient struct {
	baseURL string
	http    *http.Client
}

func newFaaSClient(baseURL string) *faasClient {
	return &faasClient{baseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second}}
}

// invokeBody mirrors models.NewRequestBody on the FaaS side.
type invokeBody struct {
	Data struct {
		AppID      string         `json:"app_id"`
		Method     string         `json:"method"`
		ObjectData map[string]any `json:"object_data"`
	} `json:"data"`
}

type invokeResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

func (f *faasClient) invoke(ctx context.Context, appID, method string, objectData map[string]any, out any) error {
	var body invokeBody
	body.Data.AppID = appID
	body.Data.Method = method
	body.Data.ObjectData = objectData

	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("invoke %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var parsed invokeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("invoke %s: bad response (status %d): %s", method, resp.StatusCode, truncate(raw, 200))
	}
	if parsed.Status != "success" {
		// error payload is {"message":...,"error":...}
		var e struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(parsed.Data, &e)
		msg := e.Message
		if msg == "" {
			msg = truncate(raw, 200)
		}
		return fmt.Errorf("invoke %s failed: %s", method, msg)
	}
	if out != nil && len(parsed.Data) > 0 {
		if err := json.Unmarshal(parsed.Data, out); err != nil {
			return fmt.Errorf("invoke %s: decode data: %w", method, err)
		}
	}
	return nil
}

// listUntranscribed asks for a batch of calls needing recognition (§5).
func (f *faasClient) listUntranscribed(ctx context.Context, appID string, limit int) ([]Call, error) {
	var out struct {
		Calls []Call `json:"calls"`
	}
	if err := f.invoke(ctx, appID, "pbx_list_untranscribed", map[string]any{"limit": limit}, &out); err != nil {
		return nil, err
	}
	return out.Calls, nil
}

// saveTranscript writes the recognition result back (pbx_save_transcript).
func (f *faasClient) saveTranscript(ctx context.Context, appID, callUUID, transcript string) error {
	return f.invoke(ctx, appID, "pbx_save_transcript", map[string]any{
		"call_uuid":  callUUID,
		"transcript": transcript,
	}, nil)
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
