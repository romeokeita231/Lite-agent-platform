package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/romeokeita231/Lite-agent-platform/internal/httpapi"
	"github.com/romeokeita231/Lite-agent-platform/internal/observability"
	"github.com/romeokeita231/Lite-agent-platform/internal/testutil"
)

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

	srv := httptest.NewServer(httpapi.NewMux(ready))
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
