// Package jev scores leads with typed questions against TypeSafe's Jev model,
// run through Cloudflare Workers AI. Jev is not asked "rate this job 0-10";
// it answers narrow Choice and Score questions with calibrated probabilities,
// and the weighting that turns those into a rank lives here, in code that can
// be read and changed without touching a prompt.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client calls one model, typesafe/jev, through the Cloudflare account-level
// ai/run endpoint.
type Client struct {
	AccountID string
	Token     string
	// BaseURL exists for tests; empty means the real Cloudflare API.
	BaseURL string
	HTTP    *http.Client
}

const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// Question is one typed question. Criteria is the type-shaped part:
// choice/noul take a map of option -> description, score takes an ordered
// list of level names.
type Question struct {
	Type         string `json:"type"` // "choice" | "score" | "noul"
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// Answer is Jev's reply to one question. Which fields are set depends on the
// question's type; Probabilities is the calibrated distribution the decision
// logic reads, never a free-text value that could be malformed.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Result is one evaluation: every answer, plus the model version and token
// usage for the audit trail.
type Result struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Evaluate sends one state and a block of questions in a single request —
// Jev's server time is flat in the number of questions, so batching them is
// both the cheap and the fast shape.
func (c *Client) Evaluate(ctx context.Context, state string, questions map[string]Question) (*Result, error) {
	base := c.BaseURL
	if base == "" {
		base = cloudflareAPI
	}
	body, err := json.Marshal(map[string]any{
		"model": "typesafe/jev",
		"input": map[string]any{"state": state, "questions": questions},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/accounts/%s/ai/run", base, c.AccountID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	httpc := c.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jev: cloudflare status %d: %s", resp.StatusCode, clipBytes(raw, 300))
	}
	// Cloudflare wraps the model output twice: the API envelope, then a run
	// envelope whose own `result` is what the Jev docs describe.
	var envelope struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result struct {
			State  string `json:"state"`
			Result Result `json:"result"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("jev: bad response: %w", err)
	}
	if !envelope.Success {
		msg := "unknown error"
		if len(envelope.Errors) > 0 {
			msg = envelope.Errors[0].Message
		}
		return nil, fmt.Errorf("jev: cloudflare error: %s", msg)
	}
	if len(envelope.Result.Result.Answers) == 0 {
		return nil, fmt.Errorf("jev: run state %q with no answers", envelope.Result.State)
	}
	return &envelope.Result.Result, nil
}

func clipBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}
