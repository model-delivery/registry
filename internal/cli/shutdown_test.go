// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// watchedStore is a store that notices when it is used after the registry
// it belongs to was closed, and that is slow to give up when cancelled, as
// a store on a network is. The slowness is what makes the test tell a
// shutdown that waits from one that only cancels.
type watchedStore struct {
	blob.Store
	closed atomic.Bool // set by the test once the registry is closed

	mu        sync.Mutex
	lateCalls []string

	backupStarted  chan struct{} // closed when a backup is being stored
	backupReturned atomic.Bool
	fetchReturned  atomic.Bool
}

// slowToStop is how long the store takes to return after it is cancelled.
const slowToStop = 150 * time.Millisecond

func (s *watchedStore) used(what string) {
	if s.closed.Load() {
		s.mu.Lock()
		s.lateCalls = append(s.lateCalls, what)
		s.mu.Unlock()
	}
}

func (s *watchedStore) Stage(ctx context.Context, r io.Reader, want blob.Expect) (blob.Staged, error) {
	s.used("Stage")
	staged, err := s.Store.Stage(ctx, r, want)
	if ctx.Err() != nil {
		time.Sleep(slowToStop)
	}
	s.used("Stage returning")
	s.fetchReturned.Store(true)
	return staged, err
}

func (s *watchedStore) PutArchive(ctx context.Context, _, _ string) error {
	s.used("PutArchive")
	close(s.backupStarted)
	<-ctx.Done()
	// Slower to stop than the fetch, so that waiting for the fetch alone
	// does not happen to outlast the backup as well.
	time.Sleep(3 * slowToStop)
	s.used("PutArchive returning")
	s.backupReturned.Store(true)
	return ctx.Err()
}

func (s *watchedStore) Delete(ctx context.Context, digest string) error {
	s.used("Delete")
	return s.Store.Delete(ctx, digest)
}

func (s *watchedStore) DeleteArchive(ctx context.Context, name string) error {
	s.used("DeleteArchive")
	return s.Store.DeleteArchive(ctx, name)
}

// heldHub is an upstream with one repository of one large file, which it
// starts to send and never finishes.
func heldHub(t *testing.T) (hubURL string, fetching chan struct{}) {
	t.Helper()
	const revision = "2222222222222222222222222222222222222222"
	const size = 1 << 20
	sum := sha256.Sum256([]byte("never sent in full"))
	fetching = make(chan struct{})
	var once sync.Once
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/acme/held/revision/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "acme/held", "sha": revision})
		case strings.HasPrefix(r.URL.Path, "/api/models/acme/held/tree/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"type": "file", "path": "model.bin", "size": size, "oid": strings.Repeat("a", 40),
				"lfs": map[string]any{"oid": hex.EncodeToString(sum[:]), "size": size},
			}})
		case strings.HasSuffix(r.URL.Path, "/model.bin"):
			_, _ = io.WriteString(w, "the first bytes, and then nothing")
			_ = http.NewResponseController(w).Flush()
			once.Do(func() { close(fetching) })
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	// Runs first: a fetch the server failed to cancel must not keep Close,
	// and with it a failing test, waiting.
	t.Cleanup(hub.CloseClientConnections)
	return hub.URL, fetching
}

// running lists the goroutines, of all there are, that are still inside a
// pull-through job or the scheduled-backup loop.
func running() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var left []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "internal/server.(*puller).do") || strings.Contains(g, "internal/cli.runBackups") {
			left = append(left, g)
		}
	}
	return left
}

// Stopping the server while a pull-through fetch and a scheduled backup are
// both in flight: both are cancelled and waited for, so that nothing is
// still using the registry when it is closed.
//
// Not parallel: it looks at every goroutine in the process, and so must
// run while no other test of this package does.
func TestServeStopsItsOwnWorkBeforeTheRegistryCloses(t *testing.T) { //nolint:paralleltest // see above.
	hubURL, fetching := heldHub(t)
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &watchedStore{Store: fs, backupStarted: make(chan struct{})}
	reg, err := registry.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := server.New(reg, server.Config{AnonymousRead: true, Upstream: hubURL, PullThrough: true, Logger: log})
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	const grace = 20 * time.Second
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	served := make(chan error, 1)
	go func() {
		served <- serve(ctx, &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}, ln, handler,
			func(ctx context.Context) { runBackups(ctx, reg, 10*time.Millisecond, 0, log) }, grace, log)
	}()

	// A client asks for a file the registry has to fetch, and waits.
	answered := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+ln.Addr().String()+"/acme/held/resolve/main/model.bin", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	for name, started := range map[string]chan struct{}{"the fetch": fetching, "the backup": store.backupStarted} {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never started", name)
		}
	}
	if len(running()) != 2 {
		t.Fatalf("expected one fetch and one backup in flight, found %d goroutines:\n%s", len(running()), strings.Join(running(), "\n\n"))
	}

	// Stop. serve must return well inside its grace period, and only after
	// the fetch and the backup have both returned.
	began := time.Now()
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(grace):
		t.Fatal("serve did not return within its grace period")
	}
	if took := time.Since(began); took > grace/2 {
		t.Fatalf("stopping took %s: the fetch and the backup were waited out, not cancelled", took)
	}
	if !store.fetchReturned.Load() || !store.backupReturned.Load() {
		t.Fatalf("serve returned while its own work was still running: fetch returned %v, backup returned %v",
			store.fetchReturned.Load(), store.backupReturned.Load())
	}
	// The client that was waiting on the fetch is told to come back.
	select {
	case status := <-answered:
		if status != http.StatusServiceUnavailable {
			t.Errorf("the waiting client was answered %d, want 503", status)
		}
	case <-time.After(5 * time.Second):
		t.Error("the waiting client was never answered")
	}

	// What cmdServe does next. From here on nothing may touch the store.
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	store.closed.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for len(running()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if left := running(); len(left) > 0 {
		t.Fatalf("%d goroutines outlived the server:\n%s", len(left), strings.Join(left, "\n\n"))
	}
	time.Sleep(4 * slowToStop) // long enough for a straggler to have shown itself
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.lateCalls) > 0 {
		t.Fatalf("the store was used after the registry closed: %v", store.lateCalls)
	}
	// The backup that was cut short stored nothing.
	if list, err := fs.ListArchives(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("backups stored = %v, %v; want none", list, err)
	}
}
