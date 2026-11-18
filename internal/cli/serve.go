// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

func cmdServe(e *env, args []string) error {
	c := newCommand(e, "serve", "serve [flags]")
	addr := c.String("addr", e.envOr("REGISTRY_ADDR", ":8080"), "listen address (env REGISTRY_ADDR)")
	data := c.String("data", e.envOr("REGISTRY_DATA", "./data"), "directory for the database and stored files (env REGISTRY_DATA)")
	upstream := c.String("upstream", e.envOr("REGISTRY_UPSTREAM", mirror.DefaultEndpoint), "hub to mirror from (env REGISTRY_UPSTREAM)")
	requireAuth := c.Bool("require-auth", envBool(e, "REGISTRY_REQUIRE_AUTH"), "require a token for downloads and browsing too (env REGISTRY_REQUIRE_AUTH)")
	storage := c.String("storage", e.envOr("REGISTRY_STORAGE", "filesystem"), "where model files are kept: filesystem or s3 (env REGISTRY_STORAGE); s3 is configured with REGISTRY_S3_* variables")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(e.stderr, nil))

	var store blob.Store // nil keeps files in the data directory
	redirect := false
	switch *storage {
	case "filesystem":
	case "s3":
		cfg := blob.S3Config{
			Endpoint:     e.getenv("REGISTRY_S3_ENDPOINT"),
			Region:       e.envOr("REGISTRY_S3_REGION", e.getenv("AWS_REGION")),
			Bucket:       e.getenv("REGISTRY_S3_BUCKET"),
			Prefix:       e.getenv("REGISTRY_S3_PREFIX"),
			AccessKey:    e.envOr("REGISTRY_S3_ACCESS_KEY", e.getenv("AWS_ACCESS_KEY_ID")),
			SecretKey:    e.envOr("REGISTRY_S3_SECRET_KEY", e.getenv("AWS_SECRET_ACCESS_KEY")),
			SessionToken: e.getenv("AWS_SESSION_TOKEN"),
			StagingDir:   filepath.Join(*data, "tmp"),
		}
		if cfg.Bucket == "" {
			return usagef("--storage s3 needs a bucket: set REGISTRY_S3_BUCKET")
		}
		s3, err := blob.NewS3(e.ctx, cfg)
		if err != nil {
			return err
		}
		store, redirect = s3, envBool(e, "REGISTRY_S3_REDIRECT")
		log.Info("storing model files in S3", "bucket", cfg.Bucket, "endpoint", cfg.Endpoint, "prefix", cfg.Prefix, "redirect_downloads", redirect)
	default:
		return usagef("unknown --storage %q: use filesystem or s3", *storage)
	}

	reg, err := registry.Open(*data, store)
	if err != nil {
		return fmt.Errorf("open data directory %s: %w", *data, err)
	}
	defer func() { _ = reg.Close() }()

	if err := bootstrapAdmin(e, reg, log); err != nil {
		return err
	}

	handler := server.New(reg, server.Config{
		AnonymousRead:     !*requireAuth,
		Upstream:          *upstream,
		UpstreamToken:     e.getenv("REGISTRY_UPSTREAM_TOKEN"),
		RedirectDownloads: redirect,
		Version:           Version,
		Logger:            log,
	})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	// No read or write timeout: model files are large and transfers are
	// long. Slow-header attacks are still bounded.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Info("registry listening", "addr", ln.Addr().String(), "data", *data, "upstream", *upstream,
		"anonymous_read", !*requireAuth, "version", Version)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-e.ctx.Done():
	}
	log.Info("shutting down; waiting for transfers in flight")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func envBool(e *env, key string) bool {
	b, _ := strconv.ParseBool(e.getenv(key))
	return b
}

// bootstrapAdmin makes sure someone can administer the registry. A token in
// REGISTRY_ADMIN_TOKEN is applied on every start; otherwise a fresh
// registry mints one and prints it once.
func bootstrapAdmin(e *env, reg *registry.Registry, log *slog.Logger) error {
	ctx := e.ctx
	if secret := e.getenv("REGISTRY_ADMIN_TOKEN"); secret != "" {
		if err := reg.SetToken(ctx, "admin", registry.RoleAdmin, secret, true); err != nil {
			return fmt.Errorf("apply REGISTRY_ADMIN_TOKEN: %w", err)
		}
		log.Info("admin token set from REGISTRY_ADMIN_TOKEN")
		return nil
	}
	toks, err := reg.ListTokens(ctx)
	if err != nil {
		return err
	}
	if len(toks) > 0 {
		return nil
	}
	secret, err := reg.CreateToken(ctx, "admin", registry.RoleAdmin)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "\nFirst start: created an admin token. It is shown only this once.\n\n    %s\n\n"+
		"Use it with:  export MODELCTL_TOKEN=%s\n\n", secret, secret)
	return nil
}
