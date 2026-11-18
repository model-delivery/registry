// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package blobtest provides an in-memory S3 service for tests, so the S3
// backend is exercised by `go test ./...` without any external service.
package blobtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/model-delivery/registry/internal/blob"
)

// FakeS3 is a running in-memory S3 service holding one bucket.
type FakeS3 struct {
	Config  blob.S3Config
	backend *s3mem.Backend
}

// NewFakeS3 starts the service and returns the configuration that reaches
// it. It is shut down when the test ends.
func NewFakeS3(t *testing.T) *FakeS3 {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("models"); err != nil {
		t.Fatal(err)
	}
	fake := gofakes3.New(backend).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The fake insists on a Content-Length header even for an empty
		// object, which Go's HTTP server does not surface for a zero-length
		// body. Real S3 services accept the request, so state the length.
		if r.Method == http.MethodPut && r.Header.Get("Content-Length") == "" && r.ContentLength <= 0 {
			r.Header.Set("Content-Length", "0")
			r.ContentLength = 0
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &FakeS3{backend: backend, Config: blob.S3Config{
		Endpoint:   srv.URL,
		Region:     "us-east-1",
		Bucket:     "models",
		Prefix:     "registry",
		AccessKey:  "test-access-key",
		SecretKey:  "test-secret-key",
		StagingDir: t.TempDir(),
	}}
}

// Store opens the S3 backend against the fake service.
func (f *FakeS3) Store(t *testing.T) *blob.S3 {
	t.Helper()
	s, err := blob.NewS3(context.Background(), f.Config)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Keys lists every object key in the bucket.
func (f *FakeS3) Keys(t *testing.T) []string {
	t.Helper()
	page, err := f.backend.ListBucket("models", nil, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range page.Contents {
		keys = append(keys, o.Key)
	}
	return keys
}
