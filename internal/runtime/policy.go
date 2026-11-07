package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
)

type ledgerStep struct {
	StepIndex int             `json:"step_index"`
	StepType  string          `json:"step_type"`
	Result    json.RawMessage `json:"result"`
}

type ledger struct {
	Recent  []ledgerStep `json:"recent"`
	Summary string       `json:"summary"`
	Dropped []int        `json:"dropped"`
}

type decisionRequest struct {
	RunID     string          `json:"run_id"`
	Agent     string          `json:"agent"`
	StepIndex int             `json:"step_index"`
	Attempt   int             `json:"attempt"`
	Input     json.RawMessage `json:"input"`
	Ledger    ledger          `json:"ledger"`
}

type action struct {
	Kind   string
	Output json.RawMessage
}

type stepFailure struct {
	reason    domain.TerminalReason
	retryable bool
	err       error
}

func (f *stepFailure) Error() string {
	if f.err == nil {
		return string(f.reason)
	}
	return fmt.Sprintf("%s: %v", f.reason, f.err)
}

func (f *stepFailure) Unwrap() error { return f.err }

type policyClient struct {
	http    *http.Client
	timeout time.Duration
}

func newPolicyClient(timeout time.Duration) *policyClient {
	return &policyClient{http: &http.Client{}, timeout: timeout}
}

func (c *policyClient) decide(ctx context.Context, agent domain.Agent, req decisionRequest) (action, error) {
	secret := os.Getenv(agent.SecretEnv)
	if secret == "" {
		return action{}, &stepFailure{
			reason: domain.ReasonPolicyUnreachable,
			err:    errors.New("policy secret is not configured"),
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return action{}, &stepFailure{reason: domain.ReasonPolicyUnreachable, err: err}
	}
	base, err := url.Parse(agent.PolicyURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return action{}, &stepFailure{
			reason: domain.ReasonPolicyUnreachable,
			err:    fmt.Errorf("invalid policy url %q", agent.PolicyURL),
		}
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/internal/policy/decide"
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature := sign(secret, http.MethodPost, base.Path, body, timestamp)

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return action{}, &stepFailure{reason: domain.ReasonPolicyUnreachable, err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Agent-Id", agent.ID)
	httpReq.Header.Set("X-Timestamp", timestamp)
	httpReq.Header.Set("X-Signature", signature)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return action{}, ctx.Err()
		}
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return action{}, &stepFailure{reason: domain.ReasonMaxRetries, retryable: true, err: err}
		}
		return action{}, &stepFailure{reason: domain.ReasonPolicyUnreachable, retryable: true, err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return action{}, &stepFailure{reason: domain.ReasonPolicyUnreachable, retryable: true, err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return action{}, &stepFailure{
			reason:    domain.ReasonPolicyUnreachable,
			retryable: true,
			err:       fmt.Errorf("policy returned status %d", resp.StatusCode),
		}
	}
	return parseAction(respBody)
}

func parseAction(body []byte) (action, error) {
	invalid := func(message string) (action, error) {
		return action{}, &stepFailure{reason: domain.ReasonPolicyInvalidAction, err: errors.New(message)}
	}
	var raw struct {
		Action json.RawMessage `json:"action"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return invalid("invalid policy response JSON")
	}
	var kind string
	if len(raw.Action) == 0 || json.Unmarshal(raw.Action, &kind) != nil || kind == "" {
		return invalid("missing action")
	}
	if kind != string(domain.StepDone) {
		return invalid("unsupported action " + kind)
	}
	output := raw.Output
	if len(output) == 0 || string(output) == "null" {
		output = json.RawMessage("{}")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(output, &object); err != nil {
		return invalid("done output must be an object")
	}
	return action{Kind: kind, Output: output}, nil
}

func sign(secret, method, path string, body []byte, timestamp string) string {
	sum := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + hex.EncodeToString(sum[:]) + "\n" + timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
