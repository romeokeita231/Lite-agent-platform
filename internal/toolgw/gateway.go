package toolgw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/romeokeita231/Lite-agent-platform/internal/domain"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
)

type Client struct {
	store      domain.Store
	timeout    time.Duration
	httpClient *http.Client
	metrics    observability.Recorder
}

func New(store domain.Store, timeout time.Duration) *Client {
	return &Client{store: store, timeout: timeout, httpClient: &http.Client{}}
}

func (c *Client) SetMetrics(metrics observability.Recorder) {
	c.metrics = metrics
}

func (c *Client) Resolve(ctx context.Context, toolName, version string) (domain.Tool, error) {
	if toolName == "" {
		return domain.Tool{}, domain.ErrNotFound
	}
	return c.store.GetTool(ctx, toolName, version)
}

func (c *Client) Validate(tool domain.Tool, args json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.Schema))
	if err != nil {
		return domain.ErrToolSchema
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("tool-schema.json", doc); err != nil {
		return domain.ErrToolSchema
	}
	compiled, err := compiler.Compile("tool-schema.json")
	if err != nil {
		return domain.ErrToolSchema
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return domain.ErrToolSchema
	}
	if err := compiled.Validate(instance); err != nil {
		return domain.ErrToolSchema
	}
	return nil
}

func (c *Client) Invoke(ctx context.Context, tool domain.Tool, args json.RawMessage, idempotencyKey string) (result json.RawMessage, err error) {
	if c.metrics != nil {
		defer func() {
			status := "ok"
			if err != nil {
				status = "error"
			}
			c.metrics.IncToolCall(status)
		}()
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, tool.Endpoint, bytes.NewReader(args))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tool endpoint returned status %d", resp.StatusCode)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return json.RawMessage("null"), nil
	}
	if !json.Valid(body) {
		return nil, errors.New("tool endpoint returned invalid JSON")
	}
	return json.RawMessage(body), nil
}
