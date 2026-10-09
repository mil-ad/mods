package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMermaidURL(t *testing.T) {
	got := mermaidURL(defaultMermaidInkServer, "graph TD\n A-->B", true, 640)
	want := "https://mermaid.ink/img/Z3JhcGggVEQKIEEtLT5C?theme=dark&type=png&width=640"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A self-hosted server, with or without a trailing slash.
	for _, base := range []string{"http://localhost:3000", "http://localhost:3000/"} {
		got := mermaidURL(base, "graph TD\n A-->B", false, 0)
		want := "http://localhost:3000/img/Z3JhcGggVEQKIEEtLT5C?type=png"
		if got != want {
			t.Errorf("base %q: got %q, want %q", base, got, want)
		}
	}
}

// TestFetchMermaidPNGQueuesAndRetries checks the two defences against
// mermaid.ink's queue limit: requests never exceed the configured concurrency at
// once, and a busy (503) response is retried rather than failing the diagram.
func TestFetchMermaidPNGQueuesAndRetries(t *testing.T) {
	defer func(d time.Duration) { mermaidRetryDelay = d }(mermaidRetryDelay)
	mermaidRetryDelay = time.Millisecond

	var active, peak, requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		// Every other request is turned away as busy, like mermaid.ink under load.
		if requests.Add(1)%2 == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("PNG"))
	}))
	defer srv.Close()

	r := &imageRenderer{
		ctx:          context.Background(),
		client:       srv.Client(),
		mermaidSlots: make(chan struct{}, defaultMermaidInkConcurrency),
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.fetchMermaidPNG(srv.URL); err != nil {
				t.Errorf("fetch: %v", err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > defaultMermaidInkConcurrency {
		t.Errorf("peak concurrency %d, want at most %d", p, defaultMermaidInkConcurrency)
	}
}

func TestFetchMermaidPNGDoesNotRetryBadSyntax(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest) // mermaid.ink's answer to invalid syntax
	}))
	defer srv.Close()

	r := &imageRenderer{
		ctx:          context.Background(),
		client:       srv.Client(),
		mermaidSlots: make(chan struct{}, defaultMermaidInkConcurrency),
	}
	if _, err := r.fetchMermaidPNG(srv.URL); err == nil {
		t.Fatal("expected an error")
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("got %d requests, want 1 (no retry)", n)
	}
}
