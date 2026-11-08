package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const signatureWindow = 5 * time.Minute

var slowToolDelay = 30 * time.Second

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func main() {
	addr := os.Getenv("POLICY_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	secret := os.Getenv("POLICY_SECRET")
	if secret == "" {
		log.Fatal("policy-example: POLICY_SECRET is required")
	}
	log.Printf("policy-example listening on %s", addr)
	if err := http.ListenAndServe(addr, newMux(secret)); err != nil {
		log.Fatalf("policy-example: %v", err)
	}
}

func newMux(secret string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /internal/policy/decide", decideHandler(secret))
	mux.HandleFunc("POST /v1/chat/completions", chatCompletionsHandler)
	mux.HandleFunc("POST /demo/tool/echo", echoHandler)
	mux.HandleFunc("POST /demo/tool/slow", slowHandler)
	return mux
}

func decideHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Id") == "" {
			http.Error(w, "missing agent id", http.StatusUnauthorized)
			return
		}
		timestamp := r.Header.Get("X-Timestamp")
		parsed, err := time.Parse(time.RFC3339, timestamp)
		if err != nil {
			http.Error(w, "invalid timestamp", http.StatusUnauthorized)
			return
		}
		if drift := time.Since(parsed); drift > signatureWindow || drift < -signatureWindow {
			http.Error(w, "timestamp outside window", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "unreadable body", http.StatusUnauthorized)
			return
		}
		expected := sign(secret, r.Method, r.URL.Path, body, timestamp)
		if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Signature"))) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var request struct {
			Agent     string          `json:"agent"`
			StepIndex int             `json:"step_index"`
			Input     json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, decideAction(request.Agent, request.StepIndex, request.Input))
	}
}

func decideAction(agent string, stepIndex int, input json.RawMessage) map[string]any {
	switch {
	case strings.HasPrefix(agent, "quick"):
		if stepIndex == 0 {
			return map[string]any{"action": "done", "output": map[string]any{"answer": "quick"}}
		}
	case strings.HasPrefix(agent, "tool-slow"):
		if stepIndex == 0 {
			return map[string]any{"action": "tool_call", "tool": "slow", "args": map[string]any{}}
		}
	case strings.HasPrefix(agent, "human"):
		if stepIndex == 0 {
			return map[string]any{"action": "human_review", "prompt": "approve?", "deadline_seconds": 60}
		}
	default:
		switch stepIndex {
		case 0:
			content := "hello"
			var body struct {
				Question string `json:"question"`
			}
			if len(input) > 0 && json.Unmarshal(input, &body) == nil && body.Question != "" {
				content = body.Question
			}
			route := os.Getenv("POLICY_MODEL")
			if route == "" {
				route = "route:default"
			}
			return map[string]any{"action": "llm_call", "route": route,
				"messages": []message{{Role: "user", Content: content}}}
		case 1:
			return map[string]any{"action": "tool_call", "tool": "echo", "args": map[string]any{"message": "hello"}}
		case 2:
			return map[string]any{"action": "human_review", "prompt": "approve?", "deadline_seconds": 60}
		}
	}
	return map[string]any{"action": "done", "output": map[string]any{"answer": "done"}}
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model    string    `json:"model"`
		Messages []message `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if request.Model == "" {
		request.Model = "demo-model"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-demo",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   request.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": "demo model reply"},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	})
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	var args json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"echo": args})
}

func slowHandler(w http.ResponseWriter, r *http.Request) {
	time.Sleep(slowToolDelay)
	writeJSON(w, http.StatusOK, map[string]any{"slept_seconds": slowToolDelay.Seconds()})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func sign(secret, method, path string, body []byte, timestamp string) string {
	sum := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + hex.EncodeToString(sum[:]) + "\n" + timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
