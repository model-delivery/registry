// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"testing"

	"github.com/model-delivery/registry/internal/blob"
)

// Every backend has a name, which is the value of the label its metrics
// carry. A store that is neither, such as one a test wraps, is "other".
func TestKind(t *testing.T) {
	t.Parallel()
	want := map[string]string{"filesystem": blob.KindFilesystem, "s3": blob.KindS3, "s3-real": blob.KindS3}
	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		got := blob.Kind(store)
		if got == blob.KindOther {
			t.Fatalf("a %T has no kind of its own: add it to blob.Kind", store)
		}
		for name, kind := range want {
			if t.Name() == "TestKind/"+name && got != kind {
				t.Fatalf("%s is named %q, want %q", name, got, kind)
			}
		}
		// Wrapped, it is no longer known as what it wraps.
		if wrapped := blob.Kind(struct{ blob.Store }{store}); wrapped != blob.KindOther {
			t.Fatalf("a wrapped store is named %q", wrapped)
		}
	})
	if got := blob.Kind(nil); got != blob.KindOther {
		t.Fatalf("no store at all is named %q", got)
	}
}
