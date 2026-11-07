package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDecideHandlerVerifiesSignatureAndWindow(t *testing.T) {
	const secret = "example-secret"
	srv := httptest.NewServer(decideHandler(secret))
	defer srv.Close()

	send := func(body []byte, timestamp, signature string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/policy/decide", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Agent-Id", "agent-1")
		req.Header.Set("X-Timestamp", timestamp)
		req.Header.Set("X-Signature", signature)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	llmBody := []byte(`{"run_id":"r1","agent":"alpha","step_index":0,"attempt":0,"input":{},"ledger":{"recent":[],"summary":"","dropped":[]}}`)
	doneBody := []byte(`{"run_id":"r1","agent":"alpha","step_index":1,"attempt":0,"input":{},"ledger":{"recent":[],"summary":"","dropped":[]}}`)
	now := time.Now().UTC().Format(time.RFC3339)

	status, raw := send(llmBody, now, sign(secret, http.MethodPost, "/internal/policy/decide", llmBody, now))
	if status != http.StatusOK {
		t.Fatalf("llm_call status = %d body=%s", status, raw)
	}
	var llmReply struct {
		Action   string    `json:"action"`
		Route    string    `json:"route"`
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &llmReply); err != nil {
		t.Fatalf("decode llm reply %s: %v", raw, err)
	}
	if llmReply.Action != "llm_call" || llmReply.Route != "route:default" {
		t.Fatalf("llm reply = %+v", llmReply)
	}
	if len(llmReply.Messages) != 1 || llmReply.Messages[0].Role != "user" || llmReply.Messages[0].Content != "hello" {
		t.Fatalf("llm messages = %+v", llmReply.Messages)
	}

	status, raw = send(doneBody, now, sign(secret, http.MethodPost, "/internal/policy/decide", doneBody, now))
	if status != http.StatusOK {
		t.Fatalf("done status = %d body=%s", status, raw)
	}
	var doneReply struct {
		Action string `json:"action"`
		Output struct {
			Answer string `json:"answer"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(raw), &doneReply); err != nil {
		t.Fatalf("decode done reply %s: %v", raw, err)
	}
	if doneReply.Action != "done" || doneReply.Output.Answer != "done" {
		t.Fatalf("done reply = %+v", doneReply)
	}

	old := time.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339)
	if status, raw := send(llmBody, old, sign(secret, http.MethodPost, "/internal/policy/decide", llmBody, old)); status != http.StatusUnauthorized {
		t.Fatalf("stale timestamp status = %d body=%s", status, raw)
	}

	if status, raw := send(llmBody, now, "deadbeef"); status != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d body=%s", status, raw)
	}
}
