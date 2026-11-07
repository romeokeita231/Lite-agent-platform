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

	body := []byte(`{"run_id":"r1","agent":"alpha","step_index":0,"attempt":0,"input":{},"ledger":{"recent":[],"summary":"","dropped":[]}}`)

	send := func(timestamp, signature string) (int, string) {
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

	now := time.Now().UTC().Format(time.RFC3339)
	status, raw := send(now, sign(secret, http.MethodPost, "/internal/policy/decide", body, now))
	if status != http.StatusOK {
		t.Fatalf("valid signature status = %d body=%s", status, raw)
	}
	var reply struct {
		Action string `json:"action"`
		Output struct {
			Answer string `json:"answer"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("decode reply %s: %v", raw, err)
	}
	if reply.Action != "done" || reply.Output.Answer != "done" {
		t.Fatalf("reply = %+v", reply)
	}

	old := time.Now().UTC().Add(-6 * time.Minute).Format(time.RFC3339)
	if status, raw := send(old, sign(secret, http.MethodPost, "/internal/policy/decide", body, old)); status != http.StatusUnauthorized {
		t.Fatalf("stale timestamp status = %d body=%s", status, raw)
	}

	if status, raw := send(now, "deadbeef"); status != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d body=%s", status, raw)
	}
}
