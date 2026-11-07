package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func signedDecide(t *testing.T, srv *httptest.Server, secret, agent string, stepIndex int, input string) map[string]any {
	t.Helper()
	body := []byte(`{"run_id":"r1","agent":"` + agent + `","step_index":` +
		strconv.Itoa(stepIndex) + `,"attempt":0,"input":` + input + `,"ledger":{"recent":[],"summary":"","dropped":[]}}`)
	now := time.Now().UTC().Format(time.RFC3339)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/policy/decide", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Agent-Id", "agent-1")
	req.Header.Set("X-Timestamp", now)
	req.Header.Set("X-Signature", sign(secret, http.MethodPost, "/internal/policy/decide", body, now))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decide %s step %d = %d body=%s", agent, stepIndex, resp.StatusCode, raw)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decode reply %s: %v", raw, err)
	}
	return reply
}

func TestDecideCoversFourActionsForDemoAgent(t *testing.T) {
	const secret = "example-secret"
	srv := httptest.NewServer(newMux(secret))
	defer srv.Close()

	llm := signedDecide(t, srv, secret, "demo", 0, `{"question":"hi"}`)
	if llm["action"] != "llm_call" || llm["route"] != "route:default" {
		t.Fatalf("llm reply = %+v", llm)
	}
	messages, _ := llm["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("llm messages = %+v", llm["messages"])
	}

	tool := signedDecide(t, srv, secret, "demo", 1, `{}`)
	if tool["action"] != "tool_call" || tool["tool"] != "echo" {
		t.Fatalf("tool reply = %+v", tool)
	}

	human := signedDecide(t, srv, secret, "demo", 2, `{}`)
	if human["action"] != "human_review" || human["prompt"] != "approve?" {
		t.Fatalf("human reply = %+v", human)
	}
	if deadline, _ := human["deadline_seconds"].(float64); deadline <= 0 {
		t.Fatalf("human deadline = %+v", human)
	}

	done := signedDecide(t, srv, secret, "demo", 3, `{}`)
	if done["action"] != "done" {
		t.Fatalf("done reply = %+v", done)
	}
}

func TestDecidePrefixedAgents(t *testing.T) {
	const secret = "example-secret"
	srv := httptest.NewServer(newMux(secret))
	defer srv.Close()

	slow := signedDecide(t, srv, secret, "tool-slow", 0, `{}`)
	if slow["action"] != "tool_call" || slow["tool"] != "slow" {
		t.Fatalf("tool-slow reply = %+v", slow)
	}
	if reply := signedDecide(t, srv, secret, "tool-slow", 1, `{}`); reply["action"] != "done" {
		t.Fatalf("tool-slow done reply = %+v", reply)
	}

	human := signedDecide(t, srv, secret, "human", 0, `{}`)
	if human["action"] != "human_review" {
		t.Fatalf("human reply = %+v", human)
	}
	if reply := signedDecide(t, srv, secret, "human", 1, `{}`); reply["action"] != "done" {
		t.Fatalf("human done reply = %+v", reply)
	}
}

func TestDecideRejectsStaleAndUnsignedRequests(t *testing.T) {
	const secret = "example-secret"
	srv := httptest.NewServer(newMux(secret))
	defer srv.Close()

	body := []byte(`{"run_id":"r1","agent":"demo","step_index":0,"attempt":0,"input":{},"ledger":{"recent":[],"summary":"","dropped":[]}}`)
	post := func(timestamp, signature string) int {
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
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	old := time.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339)
	if status := post(old, sign(secret, http.MethodPost, "/internal/policy/decide", body, old)); status != http.StatusUnauthorized {
		t.Fatalf("stale timestamp status = %d", status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if status := post(now, "deadbeef"); status != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d", status)
	}
}

func TestModelToolAndHealthEndpoints(t *testing.T) {
	previous := slowToolDelay
	slowToolDelay = 10 * time.Millisecond
	defer func() { slowToolDelay = previous }()

	srv := httptest.NewServer(newMux("example-secret"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	readyResp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	defer readyResp.Body.Close()
	if readyResp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d", readyResp.StatusCode)
	}

	chatBody := []byte(`{"model":"demo-model","messages":[{"role":"user","content":"hi"}]}`)
	chatResp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(chatBody))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer chatResp.Body.Close()
	var chat struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(chatResp.Body).Decode(&chat); err != nil {
		t.Fatalf("decode chat: %v", err)
	}
	if chat.Model != "demo-model" || len(chat.Choices) != 1 || chat.Choices[0].Message.Content == "" {
		t.Fatalf("chat reply = %+v", chat)
	}
	if chat.Usage.PromptTokens == 0 || chat.Usage.TotalTokens == 0 {
		t.Fatalf("chat usage = %+v", chat.Usage)
	}

	args := []byte(`{"message":"hello"}`)
	echoResp, err := http.Post(srv.URL+"/demo/tool/echo", "application/json", bytes.NewReader(args))
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	defer echoResp.Body.Close()
	var echo struct {
		Echo map[string]string `json:"echo"`
	}
	if err := json.NewDecoder(echoResp.Body).Decode(&echo); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echo.Echo["message"] != "hello" {
		t.Fatalf("echo reply = %+v", echo)
	}

	slowResp, err := http.Post(srv.URL+"/demo/tool/slow", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("slow: %v", err)
	}
	defer slowResp.Body.Close()
	if slowResp.StatusCode != http.StatusOK {
		t.Fatalf("slow status = %d", slowResp.StatusCode)
	}
}
