package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

const signatureWindow = 5 * time.Minute

func main() {
	addr := os.Getenv("POLICY_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	secret := os.Getenv("POLICY_SECRET")
	if secret == "" {
		log.Fatal("policy-example: POLICY_SECRET is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/policy/decide", decideHandler(secret))
	log.Printf("policy-example listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("policy-example: %v", err)
	}
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"done","output":{"answer":"done"}}`))
	}
}

func sign(secret, method, path string, body []byte, timestamp string) string {
	sum := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + hex.EncodeToString(sum[:]) + "\n" + timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
