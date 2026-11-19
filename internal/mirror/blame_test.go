// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// noRoom is a store whose disk is full: nothing can be staged.
type noRoom struct{ blob.Store }

func (noRoom) Stage(context.Context, io.Reader, blob.Expect) (blob.Staged, error) {
	return nil, errors.New("create staging file: open /srv/registry/tmp/stage-1: no space left on device")
}

// A caller that answers a client has to know whose failure a mirror's
// error is: the upstream's (reported as such) or this registry's own
// (logged, and not described to the client).
func TestFailuresAreAttributedToTheUpstreamOrToTheRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Each hub gets files of its own: the cases run at the same time, and
	// one of them changes what its hub serves.
	files := func() map[string]string {
		return map[string]string{"config.json": `{"a":1}`, "model.bin": "weights"}
	}

	prepare := func(t *testing.T, h *hub) *Plan {
		t.Helper()
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}

	t.Run("upstream", func(t *testing.T) {
		t.Parallel()
		cases := map[string]func(t *testing.T) error{
			"it cannot be reached": func(t *testing.T) error {
				h := newHub(t, files())
				h.Close()
				_, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
				return err
			},
			"it does not have the repository": func(t *testing.T) error {
				_, err := Prepare(ctx, &Upstream{Endpoint: newHub(t, files()).URL}, "acme/absent", "main")
				return err
			},
			"it answers with a server error": func(t *testing.T) error {
				h := newHub(t, files())
				h.wrap = func(w http.ResponseWriter, _ *http.Request) bool {
					http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
					return true
				}
				_, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
				return err
			},
			"its listing names a path the registry does not store": func(t *testing.T) error {
				_, err := Prepare(ctx, &Upstream{Endpoint: newHub(t, map[string]string{"../escape": "x"}).URL}, "acme/tiny", "main")
				return err
			},
			"it sends content that is not what it listed": func(t *testing.T) error {
				h := newHub(t, files())
				plan := prepare(t, h)
				h.files["model.bin"] = "tampered"
				_, err := plan.Execute(ctx, openRegistry(t), 1, nil)
				if !errors.Is(err, blob.ErrMismatch) {
					t.Fatalf("got %v, want a verification failure", err)
				}
				return err
			},
			"it answers every request for a file with an error": func(t *testing.T) error {
				h := newHub(t, files())
				plan := prepare(t, h)
				h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
					if !strings.Contains(r.URL.Path, "/resolve/") {
						return false
					}
					http.Error(w, "gone away", http.StatusBadGateway)
					return true
				}
				// Every reconnection is refused too. They are made without
				// waiting, so the test neither sleeps nor depends on a deadline
				// landing at the right moment.
				plan.up.retryWait = time.Nanosecond
				_, err := plan.Execute(ctx, openRegistry(t), 1, nil)
				return err
			},
		}
		for name, run := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				err := run(t)
				if err == nil {
					t.Fatal("no error")
				}
				if !FromUpstream(err) {
					t.Fatalf("not attributed to the upstream: %v", err)
				}
			})
		}
	})

	t.Run("registry", func(t *testing.T) {
		t.Parallel()
		// The upstream is sound; this registry cannot store what it sends.
		fs, err := blob.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		reg, err := registry.Open(t.TempDir(), noRoom{fs})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reg.Close() })
		_, err = prepare(t, newHub(t, files())).Execute(ctx, reg, 1, nil)
		if err == nil || !strings.Contains(err.Error(), "no space left") {
			t.Fatalf("got %v, want the store's failure", err)
		}
		if FromUpstream(err) {
			t.Fatalf("a full disk was attributed to the upstream: %v", err)
		}
	})

	t.Run("neither, when the caller gave up", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, files())
		stopped, cancel := context.WithCancel(ctx)
		cancel()
		_, err := Prepare(stopped, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if !errors.Is(err, context.Canceled) || FromUpstream(err) {
			t.Fatalf("got %v (from upstream: %v), want a plain cancellation", err, FromUpstream(err))
		}
	})
}

// An upstream that accepts a connection and never answers is given up on,
// while one that answers and then sends slowly is not cut off.
func TestUpstreamTimeouts(t *testing.T) {
	t.Parallel()

	t.Run("the default client bounds connecting and waiting, not the transfer", func(t *testing.T) {
		t.Parallel()
		tr, ok := guardedClient.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("the default transport is a %T", guardedClient.Transport)
		}
		if upstreamTimeouts.dial <= 0 || tr.TLSHandshakeTimeout <= 0 || tr.ResponseHeaderTimeout <= 0 {
			t.Fatalf("an unbounded wait: dial %s, TLS handshake %s, response header %s",
				upstreamTimeouts.dial, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
		}
		if guardedClient.Timeout != 0 {
			t.Fatalf("the client limits a whole request to %s, which would cut large files off", guardedClient.Timeout)
		}
	})

	const wait = 100 * time.Millisecond
	client := &http.Client{Transport: guardedTransport(timeouts{dial: 5 * time.Second, tlsHandshake: 5 * time.Second, responseHeader: wait})}

	t.Run("a host that never answers", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		silent := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		t.Cleanup(silent.Close)
		t.Cleanup(func() { close(release) })

		started := time.Now()
		_, err := Prepare(context.Background(), &Upstream{Endpoint: silent.URL, Client: client}, "acme/tiny", "main")
		if err == nil || !FromUpstream(err) {
			t.Fatalf("got %v, want an upstream failure", err)
		}
		if took := time.Since(started); took > 50*wait {
			t.Fatalf("gave up after %s; the limit is %s", took, wait)
		}
	})

	t.Run("a host that answers at once and sends slowly", func(t *testing.T) {
		t.Parallel()
		const content = "slow but steady"
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			time.Sleep(3 * wait) // longer than the wait for an answer to begin
			_, _ = io.WriteString(w, content)
		}))
		t.Cleanup(slow.Close)

		body := (&Upstream{Endpoint: slow.URL, Client: client}).open(context.Background(), "acme/tiny", hubSHA, "model.bin")
		defer func() { _ = body.Close() }()
		got, err := io.ReadAll(body)
		if err != nil || string(got) != content {
			t.Fatalf("read %q, %v; a transfer in progress was cut off", got, err)
		}
	})
}
