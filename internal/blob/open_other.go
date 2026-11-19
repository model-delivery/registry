// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package blob

import "os"

// openShared opens a staging file for reading while it is still being
// written, in a way that does not stop it from being renamed or removed.
// Outside Windows every open is like that.
func openShared(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // a staging file this store just created.
}
