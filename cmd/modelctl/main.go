// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Command modelctl is the Model Delivery registry: one binary that runs
// the server (modelctl serve) and drives it (mirror, push, pull, ...).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/model-delivery/registry/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}
