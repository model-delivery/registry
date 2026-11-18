// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/datadir"
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
	requireAdmin := c.Bool("require-admin-token", envBool(e, "REGISTRY_REQUIRE_ADMIN_TOKEN"), "refuse to start without a supplied admin token instead of generating one and printing it (env REGISTRY_REQUIRE_ADMIN_TOKEN); use where logs are collected")
	pullThrough := c.Bool("pull-through", envBool(e, "REGISTRY_PULL_THROUGH"), "fetch a model from the upstream hub the first time a client asks for one the registry does not hold (env REGISTRY_PULL_THROUGH); limit which with REGISTRY_PULL_THROUGH_ALLOW")
	storage := c.String("storage", e.envOr("REGISTRY_STORAGE", "filesystem"), "where model files are kept: filesystem or s3 (env REGISTRY_STORAGE); s3 is configured with REGISTRY_S3_* variables")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(e.stderr, nil))

	backupEvery, backupKeep, err := backupSettings(e)
	if err != nil {
		return err
	}
	// This process owns the data directory from here on, so whatever a
	// previous run left half-received is cleared, once, before anything
	// new is staged. No other command does this.
	if err := datadir.Dir(*data).ClearStaging(); err != nil {
		return err
	}
	store, redirect, err := openStore(e, *data, *storage, log)
	if err != nil {
		return err
	}
	if envBool(e, "REGISTRY_RESTORE_IF_EMPTY") {
		if err := restoreIfEmpty(e, *data, store, log); err != nil {
			return err
		}
	}

	reg, err := registry.Open(*data, store)
	if err != nil {
		return fmt.Errorf("open data directory %s: %w", *data, err)
	}
	defer func() { _ = reg.Close() }()

	if err := bootstrapAdmin(e, reg, log, *requireAdmin); err != nil {
		return err
	}

	var allow []string
	for _, pattern := range strings.Split(e.getenv("REGISTRY_PULL_THROUGH_ALLOW"), ",") {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			if _, err := path.Match(pattern, "a/b"); err != nil {
				return usagef("REGISTRY_PULL_THROUGH_ALLOW: %q is not a valid pattern", pattern)
			}
			allow = append(allow, pattern)
		}
	}
	if *pullThrough && !*requireAuth && len(allow) == 0 {
		// Anyone who can reach the registry can then make it download any
		// public model, which is fine on a private network and not on a public one.
		log.Warn("pull-through is on with open downloads and no allow list: anyone who can reach this registry can make it fetch and store any upstream model; set REGISTRY_REQUIRE_AUTH or REGISTRY_PULL_THROUGH_ALLOW unless the network is private")
	}

	pullMaxFile, err := envBytes(e, "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES")
	if err != nil {
		return err
	}
	maxBlob, err := envBytes(e, "REGISTRY_MAX_BLOB_BYTES")
	if err != nil {
		return err
	}

	handler := server.New(reg, server.Config{
		AnonymousRead:     !*requireAuth,
		Upstream:          *upstream,
		UpstreamToken:     e.getenv("REGISTRY_UPSTREAM_TOKEN"),
		RedirectDownloads: redirect,
		PullThrough:       *pullThrough,
		PullThroughAllow:  allow,
		BackupKeep:        backupKeep,
		Version:           Version,
		Logger:            log,

		PullThroughMaxFileBytes: pullMaxFile,
		MaxBlobBytes:            maxBlob,
	})
	ln, err := (&net.ListenConfig{}).Listen(e.ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	// No read or write timeout: model files are large and transfers are
	// long. Slow-header attacks are still bounded, and the handlers of the
	// small JSON and form routes set a deadline for their own bodies.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Info("registry listening", "addr", ln.Addr().String(), "data", *data, "upstream", *upstream,
		"anonymous_read", !*requireAuth, "pull_through", *pullThrough, "version", Version)

	if backupEvery > 0 {
		log.Info("scheduled metadata backups are on", "every", backupEvery.String(), "keep", backupKeep)
		go runBackups(e.ctx, reg, backupEvery, backupKeep, log)
	}

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

// openStore builds the storage backend the configuration names.
func openStore(e *env, data, storage string, log *slog.Logger) (store blob.Store, redirect bool, err error) {
	switch storage {
	case "filesystem":
		fs, err := blob.NewFS(data)
		return fs, false, err
	case "s3":
		cfg := blob.S3Config{
			Endpoint:     e.getenv("REGISTRY_S3_ENDPOINT"),
			Region:       e.envOr("REGISTRY_S3_REGION", e.getenv("AWS_REGION")),
			Bucket:       e.getenv("REGISTRY_S3_BUCKET"),
			Prefix:       e.getenv("REGISTRY_S3_PREFIX"),
			AccessKey:    e.envOr("REGISTRY_S3_ACCESS_KEY", e.getenv("AWS_ACCESS_KEY_ID")),
			SecretKey:    e.envOr("REGISTRY_S3_SECRET_KEY", e.getenv("AWS_SECRET_ACCESS_KEY")),
			SessionToken: e.getenv("AWS_SESSION_TOKEN"),
			StagingDir:   datadir.Dir(data).Staging(),
		}
		if cfg.Bucket == "" {
			return nil, false, usagef("--storage s3 needs a bucket: set REGISTRY_S3_BUCKET")
		}
		s3, err := blob.NewS3(e.ctx, cfg)
		if err != nil {
			return nil, false, err
		}
		redirect = envBool(e, "REGISTRY_S3_REDIRECT")
		log.Info("storing model files in S3", "bucket", cfg.Bucket, "endpoint", cfg.Endpoint, "prefix", cfg.Prefix, "redirect_downloads", redirect)
		return s3, redirect, nil
	}
	return nil, false, usagef("unknown --storage %q: use filesystem or s3", storage)
}

// backupSettings reads the schedule for metadata backups: how often, and
// how many to keep.
func backupSettings(e *env) (every time.Duration, keep int, err error) {
	keep = 48
	if v := e.getenv("REGISTRY_BACKUP_KEEP"); v != "" {
		keep, err = strconv.Atoi(v)
		if err != nil || keep < 0 {
			return 0, 0, usagef("REGISTRY_BACKUP_KEEP must be a number, 0 to keep every backup; got %q", v)
		}
	}
	if v := e.getenv("REGISTRY_BACKUP_INTERVAL"); v != "" {
		every, err = time.ParseDuration(v)
		if err != nil || every < time.Minute {
			return 0, 0, usagef("REGISTRY_BACKUP_INTERVAL must be a duration of at least a minute, such as 1h or 30m; got %q", v)
		}
	}
	return every, keep, nil
}

// runBackups takes a metadata backup every interval until ctx ends. A
// failed backup is logged and the schedule carries on.
func runBackups(ctx context.Context, reg *registry.Registry, every time.Duration, keep int, log *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			res, err := reg.Backup(ctx, keep)
			if err != nil {
				log.Error("scheduled metadata backup failed", "error", err)
				continue
			}
			log.Info("metadata backup stored", "name", res.Backup.Name, "bytes", res.Backup.Size,
				"repos", res.Contents.Repos, "revisions", res.Contents.Revisions)
			if res.PruneErr != nil {
				log.Warn("the backup was stored, but old backups beyond the number to keep could not be removed", "keep", keep, "error", res.PruneErr)
			}
		}
	}
}

// restoreIfEmpty installs the newest stored backup when the data directory
// has no database. It is for a registry whose disk does not outlive it: a
// replacement starts from the last backup instead of from nothing. A data
// directory that already has a database is never touched.
func restoreIfEmpty(e *env, data string, store blob.Store, log *slog.Logger) error {
	if _, err := os.Stat(datadir.Dir(data).Database()); err == nil {
		return nil
	}
	res, err := registry.RestoreBackup(e.ctx, data, store, "", false)
	switch {
	case errors.Is(err, blob.ErrArchiveNotFound):
		log.Info("no database and no stored backup: starting as a new registry")
		return nil
	case err != nil:
		return fmt.Errorf("REGISTRY_RESTORE_IF_EMPTY: restore the newest backup: %w", err)
	}
	log.Info("restored the metadata database from the newest stored backup", "backup", res.Backup,
		"repos", res.Contents.Repos, "revisions", res.Contents.Revisions)
	return nil
}

// envBytes reads an optional size limit in bytes; unset or 0 means none.
func envBytes(e *env, key string) (int64, error) {
	v := e.getenv(key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, usagef("%s must be a whole number of bytes, 0 for no limit; got %q", key, v)
	}
	return n, nil
}

func envBool(e *env, key string) bool {
	b, _ := strconv.ParseBool(e.getenv(key))
	return b
}

// recommendedAdminTokenLen is the length below which a supplied admin token
// draws a warning at start. `openssl rand -hex 24` gives 48 characters.
const recommendedAdminTokenLen = 32

// bootstrapAdmin makes sure someone can administer the registry. A token in
// REGISTRY_ADMIN_TOKEN is applied on every start; otherwise a fresh
// registry mints one and prints it once.
//
// Where logs are collected (a container platform), printing a token puts a
// credential in the log store. There, supply the token (from a mounted
// secret with REGISTRY_ADMIN_TOKEN_FILE, or from REGISTRY_ADMIN_TOKEN) and
// set requireSupplied, which refuses to start rather than mint and print.
func bootstrapAdmin(e *env, reg *registry.Registry, log *slog.Logger, requireSupplied bool) error {
	ctx := e.ctx
	secret, source := e.getenv("REGISTRY_ADMIN_TOKEN"), "REGISTRY_ADMIN_TOKEN"
	if file := e.getenv("REGISTRY_ADMIN_TOKEN_FILE"); file != "" {
		if secret != "" {
			return usagef("set REGISTRY_ADMIN_TOKEN or REGISTRY_ADMIN_TOKEN_FILE, not both")
		}
		raw, err := os.ReadFile(file) //nolint:gosec // G304: the operator names this file in the server's own configuration.
		if err != nil {
			return fmt.Errorf("read REGISTRY_ADMIN_TOKEN_FILE: %w", err)
		}
		// Secret files usually end with a newline; it is not part of the token.
		secret, source = strings.TrimSpace(string(raw)), "REGISTRY_ADMIN_TOKEN_FILE"
		if secret == "" {
			return fmt.Errorf("REGISTRY_ADMIN_TOKEN_FILE %s is empty", file)
		}
	}
	if secret != "" {
		if err := reg.SetToken(ctx, "admin", registry.RoleAdmin, secret, true); err != nil {
			return fmt.Errorf("apply %s: %w", source, err)
		}
		log.Info("admin token set from " + source)
		if len(secret) < recommendedAdminTokenLen {
			// Shorter tokens are still accepted, as they always were. The
			// length matters because the token's hash is in the database
			// and in every backup, where candidates can be tried offline.
			log.Warn(fmt.Sprintf("the admin token from %s is shorter than %d characters: use a long random one, for example from `openssl rand -hex 24`; "+
				"its hash is stored in the database and in every backup, where a short or guessable token can be found by trying candidates",
				source, recommendedAdminTokenLen))
		}
		return nil
	}
	toks, err := reg.ListTokens(ctx)
	if err != nil {
		return err
	}
	if len(toks) > 0 {
		return nil
	}
	if requireSupplied {
		return usagef("this registry has no access tokens and none was supplied: set REGISTRY_ADMIN_TOKEN_FILE or REGISTRY_ADMIN_TOKEN (REGISTRY_REQUIRE_ADMIN_TOKEN is on, so one will not be generated and printed)")
	}
	secret, err = reg.CreateToken(ctx, "admin", registry.RoleAdmin)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stderr, "\nFirst start: created an admin token. It is shown only this once.\n\n    %s\n\n"+
		"Use it with:  export MODELCTL_TOKEN=%s\n\n", secret, secret)
	return nil
}
