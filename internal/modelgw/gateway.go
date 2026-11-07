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
)

type Client struct {
	baseURL    string
	apiKey     string
	budget     time.Duration
	httpClient *http.Client
}

func New(baseURL, apiKey string, budget time.Duration) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		budget:     budget,
		httpClient: &http.Client{},
	}
}

func (c *Client) Generate(ctx context.Context, req domain.ModelRequest) (domain.ModelResult, error) {
	model := req.Route
	result := domain.ModelResult{
		RouteLabel:     model,
		RequestedModel: model,
		Model:          model,
		Chain:          []string{model},
	}
	started := time.Now()
	fail := func(err error) (domain.ModelResult, error) {
		result.LatencyMS = time.Since(started).Milliseconds()
		result.Status = domain.ModelStatusError
		return result, err
	}

	if c.baseURL == "" {
		return fail(errors.New("MODEL_BASE_URL is not configured"))
	}
	body, err := json.Marshal(struct {
		Model    string                `json:"model"`
		Messages []domain.ModelMessage `json:"messages"`
	}{Model: model, Messages: req.Messages})
	if err != nil {
		return fail(err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		strings.TrimSuffix(c.baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fail(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(fmt.Errorf("model upstream returned status %d", resp.StatusCode))
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
		return fail(err)
	}
	if len(decoded.Choices) == 0 {
		return fail(errors.New("model upstream returned no choices"))
	}
	var content string
	if len(decoded.Choices[0].Message.Content) > 0 {
		if err := json.Unmarshal(decoded.Choices[0].Message.Content, &content); err != nil {
			return fail(err)
		}
	}

	result.Content = content
	result.InputTokens = decoded.Usage.PromptTokens
	result.OutputTokens = decoded.Usage.CompletionTokens
	result.LatencyMS = time.Since(started).Milliseconds()
	result.Status = domain.ModelStatusOK
	return result, nil
}
