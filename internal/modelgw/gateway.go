package modelgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
)

type Client struct {
	store      domain.Store
	baseURL    string
	apiKey     string
	budget     time.Duration
	httpClient *http.Client
	metrics    observability.Recorder
}

func New(baseURL, apiKey string, budget time.Duration) *Client {
	return NewWithStore(nil, baseURL, apiKey, budget)
}

func NewWithStore(store domain.Store, baseURL, apiKey string, budget time.Duration) *Client {
	return &Client{
		store:      store,
		baseURL:    baseURL,
		apiKey:     apiKey,
		budget:     budget,
		httpClient: &http.Client{},
	}
}

func (c *Client) SetMetrics(metrics observability.Recorder) {
	c.metrics = metrics
}

func (c *Client) Generate(ctx context.Context, req domain.ModelRequest) (domain.ModelResult, error) {
	requested := req.Route
	result := domain.ModelResult{
		RouteLabel:     requested,
		RequestedModel: requested,
		Status:         domain.ModelStatusError,
	}
	chain, err := c.resolveChain(ctx, requested)
	if err != nil {
		return result, err
	}
	result.Chain = chain
	if len(chain) == 0 {
		return result, fmt.Errorf("%w: empty chain for %q", domain.ErrModelRouteNotFound, requested)
	}

	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	deadline, _ := callCtx.Deadline()

	var lastFailed string
	var lastLatency time.Duration
	var lastErr error
	for index, model := range chain {
		if index > 0 && time.Until(deadline) <= lastLatency {
			break
		}
		if callCtx.Err() != nil {
			break
		}
		if index > 0 && c.metrics != nil {
			c.metrics.IncModelFallback()
		}
		attemptStarted := time.Now()
		content, inputTokens, outputTokens, callErr := c.callModel(callCtx, model, req)
		attemptEnded := time.Now()
		if c.metrics != nil {
			c.metrics.ObserveModelCallDuration(attemptEnded.Sub(attemptStarted).Seconds())
		}
		attempt := domain.ModelAttempt{
			Model:     model,
			Attempt:   index + 1,
			StartedAt: attemptStarted,
			EndedAt:   attemptEnded,
			LatencyMS: attemptEnded.Sub(attemptStarted).Milliseconds(),
		}
		if callErr == nil {
			attempt.Status = domain.ModelStatusOK
			result.Attempts = append(result.Attempts, attempt)
			result.Model = model
			result.FallbackFrom = lastFailed
			result.Content = content
			result.InputTokens = inputTokens
			result.OutputTokens = outputTokens
			result.LatencyMS = time.Since(started).Milliseconds()
			result.Status = domain.ModelStatusOK
			return result, nil
		}
		attempt.Status = domain.ModelStatusError
		attempt.Error = callErr.Error()
		result.Attempts = append(result.Attempts, attempt)
		lastErr = callErr
		lastFailed = model
		lastLatency = attemptEnded.Sub(attemptStarted)
		if !retryable(callErr) {
			result.LatencyMS = time.Since(started).Milliseconds()
			return result, callErr
		}
		if ctx.Err() != nil {
			result.LatencyMS = time.Since(started).Milliseconds()
			return result, ctx.Err()
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: no attempt ran for %q", domain.ErrModelRouteNotFound, requested)
	}
	return result, lastErr
}

func (c *Client) resolveChain(ctx context.Context, requested string) ([]string, error) {
	if !strings.HasPrefix(requested, "route:") {
		return []string{requested}, nil
	}
	label := strings.TrimPrefix(requested, "route:")
	if label == "" {
		return nil, fmt.Errorf("%w: empty label", domain.ErrModelRouteNotFound)
	}
	if c.store == nil {
		return []string{requested}, nil
	}
	route, err := c.store.GetModelRoute(ctx, label)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", domain.ErrModelRouteNotFound, label)
	}
	if err != nil {
		return nil, err
	}
	return route.Chain, nil
}

type upstreamError struct {
	err       error
	retryable bool
}

func (e *upstreamError) Error() string { return e.err.Error() }

func (e *upstreamError) Unwrap() error { return e.err }

func retryable(err error) bool {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		return upstream.retryable
	}
	return false
}

func (c *Client) callModel(ctx context.Context, model string, req domain.ModelRequest) (string, int, int, error) {
	if c.baseURL == "" {
		return "", 0, 0, &upstreamError{err: errors.New("MODEL_BASE_URL is not configured"), retryable: false}
	}
	payload := make(map[string]json.RawMessage, len(req.Extra)+2)
	for key, value := range req.Extra {
		if key == "stream" || key == "model" || key == "messages" {
			continue
		}
		payload[key] = value
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: false}
	}
	messagesJSON, err := json.Marshal(req.Messages)
	if err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: false}
	}
	payload["model"] = modelJSON
	payload["messages"] = messagesJSON
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: false}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: false}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: true}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusErr := &upstreamError{
			err:       fmt.Errorf("model upstream returned status %d", resp.StatusCode),
			retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		}
		return "", 0, 0, statusErr
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return "", 0, 0, &upstreamError{err: err, retryable: false}
	}
	if len(decoded.Choices) == 0 {
		return "", 0, 0, &upstreamError{err: errors.New("model upstream returned no choices"), retryable: false}
	}
	var content string
	if len(decoded.Choices[0].Message.Content) > 0 {
		if err := json.Unmarshal(decoded.Choices[0].Message.Content, &content); err != nil {
			return "", 0, 0, &upstreamError{err: err, retryable: false}
		}
	}
	return content, decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens, nil
}
