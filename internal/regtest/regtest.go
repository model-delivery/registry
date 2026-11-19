// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package regtest holds what the registry's tests would otherwise each
// write for themselves: a registry served over HTTP, `modelctl serve`
// started on a free port, the environment of a server on the fake S3
// service, and a directory of files to push.
//
// It imports the server, so tests inside internal/registry, internal/blob
// and internal/mirror cannot use it; what those need is small and lives in
// internal/kit (SHA256Hex) or internal/blob/blobtest.
package regtest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// stopTimeout is how long a helper waits for a server it started to stop
// its own work when the test ends. Stopping cancels that work, so this is
// reached only when something ignores its cancellation.
const stopTimeout = 30 * time.Second

// Instance is a registry served over HTTP for a test, with a token of each
// role.
type Instance struct {
	Reg *registry.Registry
	// Data is the registry's data directory.
	Data string
	URL  string
	// Admin, Write and Read are the secrets of tokens with those roles.
	Admin, Write, Read string
	// stop shuts the server down; it does so once however often it is called.
	stop func()
}

// Start serves a new registry that keeps its files on local disk.
func Start(t testing.TB, cfg server.Config) *Instance {
	t.Helper()
	return StartWith(t, cfg, nil)
}

// StartWith is Start with a chosen storage backend; nil means local files.
// When the test ends the server's own work is stopped and waited for, and
// then the registry is closed, in the order `modelctl serve` does it.
func StartWith(t testing.TB, cfg server.Config, store blob.Store) *Instance {
	t.Helper()
	in := &Instance{Data: t.TempDir()}
	reg, err := registry.Open(in.Data, store)
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the server's cleanup, so it runs after it.
	t.Cleanup(func() { _ = reg.Close() })
	in.Reg = reg
	for role, secret := range map[registry.Role]*string{registry.RoleAdmin: &in.Admin, registry.RoleWrite: &in.Write, registry.RoleRead: &in.Read} {
		if *secret, err = reg.CreateToken(context.Background(), string(role), role); err != nil {
			t.Fatal(err)
		}
	}
	in.URL, in.stop = serve(t, reg, cfg)
	return in
}

// StopServing shuts the instance's server down before the test ends, for a
// test about what happens once it is gone. The registry stays open.
func (in *Instance) StopServing() { in.stop() }

// Serve serves a registry the caller holds, with the given configuration,
// and returns the server's URL. It is how a test serves one registry a
// second time, configured differently. The server is shut down when the
// test ends; closing the registry is left to whoever opened it.
func Serve(t testing.TB, reg *registry.Registry, cfg server.Config) string {
	t.Helper()
	url, _ := serve(t, reg, cfg)
	return url
}

// serve is Serve, and also returns the function that shuts the server
// down, which the test's cleanup calls if nothing has by then.
func serve(t testing.TB, reg *registry.Registry, cfg server.Config) (url string, stop func()) {
	t.Helper()
	handler := server.New(reg, cfg)
	srv := httptest.NewServer(handler)
	stop = sync.OnceFunc(func() {
		// The server's own work first: a request may be waiting on it, and
		// closing the listener waits for requests.
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		if err := handler.Shutdown(ctx); err != nil {
			t.Errorf("the server did not stop its own work: %v", err)
		}
		srv.Close()
	})
	t.Cleanup(stop)
	return srv.URL, stop
}

// LogBuffer collects what a server logs while the test reads it.
type LogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *LogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *LogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// RunFunc is the signature of cli.Run. RunServe takes it as an argument
// because the tests that need it are inside internal/cli, which this
// package therefore cannot import.
type RunFunc func(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int

var listenAddr = regexp.MustCompile(`addr=(127\.0\.0\.1:\d+)`)

const (
	// startTimeout is how long RunServe waits for the server to say where
	// it listens, or to exit.
	startTimeout = 10 * time.Second
	// startPoll is how often it looks at the log meanwhile.
	startPoll = 20 * time.Millisecond
)

// RunServe starts `modelctl serve` on a free port with data as its data
// directory and env as its environment. It returns the server's log, the
// address it listens on, and a function that stops it and returns its exit
// code. If the server exits instead of starting, addr is empty and stop
// returns the code it exited with. A server still running when the test
// ends is stopped then.
func RunServe(t testing.TB, run RunFunc, data string, env map[string]string) (logs *LogBuffer, addr string, stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logs = &LogBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--data", data}, io.Discard, logs,
			func(k string) string { return env[k] })
	}()
	var once sync.Once
	var code int
	stop = func() int {
		once.Do(func() {
			cancel()
			code = <-exited
		})
		return code
	}
	t.Cleanup(func() { stop() })

	deadline := time.Now().Add(startTimeout)
	for {
		if m := listenAddr.FindStringSubmatch(logs.String()); m != nil {
			return logs, m[1], stop
		}
		select {
		case c := <-exited:
			exited <- c // for stop
			return logs, "", stop
		case <-time.After(startPoll):
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve neither started nor exited:\n%s", logs.String())
		}
	}
}

// Status sends a GET for url, with token if it is not empty, and returns
// the status of the answer.
func Status(t testing.TB, url, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// S3Prefix is the key prefix S3Env configures, so a test can tell that the
// server used it.
const S3Prefix = "prod"

// S3Env is the environment of a server that keeps its files on the fake S3
// service, with adminToken as its admin token. extra is added to it.
func S3Env(fake *blobtest.FakeS3, adminToken string, extra map[string]string) map[string]string {
	env := map[string]string{
		"REGISTRY_STORAGE":       "s3",
		"REGISTRY_S3_ENDPOINT":   fake.Config.Endpoint,
		"REGISTRY_S3_BUCKET":     fake.Config.Bucket,
		"REGISTRY_S3_PREFIX":     S3Prefix,
		"REGISTRY_S3_ACCESS_KEY": fake.Config.AccessKey,
		"REGISTRY_S3_SECRET_KEY": fake.Config.SecretKey,
		"REGISTRY_S3_REGION":     fake.Config.Region,
		"REGISTRY_ADMIN_TOKEN":   adminToken,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// WriteTree writes files, given as slash-separated path to content, under
// a new temporary directory and returns it.
func WriteTree(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// FileSHA256 is the SHA-256, in hex, of the file at path.
func FileSHA256(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a path the test chose.
	if err != nil {
		t.Fatal(err)
	}
	return kit.SHA256Hex(b)
}
