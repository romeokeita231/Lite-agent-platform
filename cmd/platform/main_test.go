package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

func TestKeygenPrintsPlaintextHashAndPrefixOnce(t *testing.T) {
	var buf bytes.Buffer
	if err := keygen(&buf); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(buf.String()), "\n"); len(lines) != 1 {
		t.Fatalf("keygen output is not one line: %q", buf.String())
	}
	var out struct {
		APIKey       string `json:"api_key"`
		APIKeyHash   string `json:"api_key_hash"`
		APIKeyPrefix string `json:"api_key_prefix"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode keygen output: %v", err)
	}
	if out.APIKey == "" || len(out.APIKeyPrefix) != 8 {
		t.Fatalf("keygen output = %+v", out)
	}
	sum := sha256.Sum256([]byte(out.APIKey))
	if want := hex.EncodeToString(sum[:]); out.APIKeyHash != want {
		t.Fatalf("api_key_hash = %q, want %q", out.APIKeyHash, want)
	}
	if out.APIKeyPrefix != out.APIKey[:8] {
		t.Fatalf("api_key_prefix = %q, want %q", out.APIKeyPrefix, out.APIKey[:8])
	}

	var second bytes.Buffer
	if err := keygen(&second); err != nil {
		t.Fatalf("second keygen: %v", err)
	}
	if second.String() == buf.String() {
		t.Fatalf("keygen repeated the same key")
	}
}

func TestPlatformStaysAliveWhenDatabaseUnreachable(t *testing.T) {
	bin := buildPlatform(t)
	addr := recoveryFreeAddr(t)
	dbURL := "postgres://postgres:postgres@" + recoveryFreeAddr(t) + "/platform?sslmode=disable&connect_timeout=1"
	startPlatform(t, bin, dbURL, addr, "unused-secret", "http://127.0.0.1:1/v1")
	base := "http://" + addr

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz never became available")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if status := getStatus(t, base+"/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unreachable database = %d, want 503", status)
	}

	time.Sleep(2 * time.Second)
	if status := getStatus(t, base+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz after database outage = %d, want 200", status)
	}
	if status := getStatus(t, base+"/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("readyz after database outage = %d, want 503", status)
	}
}

func TestReadinessFlipsWithDatabaseRecovery(t *testing.T) {
	dbURL := testutil.URL(t)
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}

	proxy := newTCPProxy(t, parsed.Host)
	proxy.SetForwarding(false)

	proxyURL := *parsed
	proxyURL.Host = proxy.Addr()
	pool, err := pgxpool.New(context.Background(), proxyURL.String())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	ready := observability.NewReadiness()
	maint := &dbMaintenance{pool: pool, ready: ready}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	maint.check(ctx)
	if ready.Ready() {
		t.Fatal("readiness is true while the database is unreachable")
	}

	srv := httptest.NewServer(httpapi.NewMux(ready, nil, nil, ready, nil))
	defer srv.Close()

	if status := getStatus(t, srv.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", status)
	}
	if status := getStatus(t, srv.URL+"/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", status)
	}

	go maint.loop(ctx, 20*time.Millisecond)
	proxy.SetForwarding(true)

	waitStatus(t, srv.URL+"/readyz", http.StatusOK, 10*time.Second)

	if status := getStatus(t, srv.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", status)
	}
}

func getStatus(t *testing.T, target string) int {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func waitStatus(t *testing.T, target string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := 0
	for time.Now().Before(deadline) {
		last = getStatus(t, target)
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s = %d, want %d within %s", target, last, want, timeout)
}

type tcpProxy struct {
	upstream string
	listener net.Listener

	mu         sync.Mutex
	forwarding bool
	conns      map[net.Conn]struct{}
}

func newTCPProxy(t *testing.T, upstream string) *tcpProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxy := &tcpProxy{
		upstream: upstream,
		listener: listener,
		conns:    make(map[net.Conn]struct{}),
	}
	go proxy.accept()
	t.Cleanup(proxy.Close)
	return proxy
}

func (p *tcpProxy) Addr() string {
	return p.listener.Addr().String()
}

func (p *tcpProxy) SetForwarding(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forwarding = on
	if !on {
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.conns = make(map[net.Conn]struct{})
	}
}

func (p *tcpProxy) Close() {
	_ = p.listener.Close()
	p.SetForwarding(false)
}

func (p *tcpProxy) accept() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *tcpProxy) handle(client net.Conn) {
	if !p.track(client) {
		_ = client.Close()
		return
	}
	upstream, err := net.Dial("tcp", p.upstream)
	if err != nil {
		p.untrack(client)
		_ = client.Close()
		return
	}
	if !p.track(upstream) {
		p.untrack(client)
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	go p.pipe(upstream, client)
	p.pipe(client, upstream)
	p.untrack(client)
	p.untrack(upstream)
	_ = client.Close()
	_ = upstream.Close()
}

func (p *tcpProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.forwarding {
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}

func (p *tcpProxy) untrack(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, conn)
}

func (p *tcpProxy) pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}
