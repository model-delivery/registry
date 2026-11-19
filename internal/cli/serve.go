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

// How the server is configured.
//
// Every setting that is not a secret has a flag and an environment
// variable: the flag wins, and the variable is the flag's default, which
// is why --help shows the value in effect. Secrets (the admin token, the
// upstream token, the S3 keys) are read from the environment only, because
// a command line shows in the process list and in shell history.

// Defaults of the server's settings.
const (
	defaultAddr    = ":8080"
	defaultDataDir = "./data"
	// defaultBackupKeep is how many scheduled backups are kept: two days of
	// hourly ones.
	defaultBackupKeep = 48
	// minBackupInterval is the most often scheduled backups may run.
	minBackupInterval = time.Minute
)

// The kinds of storage for model files.
const (
	storageFilesystem = "filesystem"
	storageS3         = "s3"
)

// What --help says about the settings that have no flag.
const (
	restoreEnvOnlyNote = `S3 keys are read from the environment only, never from flags, because a
command line shows in the process list and in shell history:
  REGISTRY_S3_ACCESS_KEY, REGISTRY_S3_SECRET_KEY
      (or AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN)
`
	serveEnvOnlyNote = `Secrets are read from the environment only, never from flags, because a
command line shows in the process list and in shell history:
  REGISTRY_ADMIN_TOKEN        the admin token, applied at every start
  REGISTRY_ADMIN_TOKEN_FILE   or a file that holds it, such as a mounted secret
  REGISTRY_UPSTREAM_TOKEN     token for the upstream hub, for private and gated models
  REGISTRY_S3_ACCESS_KEY, REGISTRY_S3_SECRET_KEY
      (or AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN)
`
)

func cmdServe(e *env, args []string) error {
	c := newCommand(e, "serve", "serve [flags]")
	c.notes = serveEnvOnlyNote
	addr := c.String("addr", e.envOr("REGISTRY_ADDR", defaultAddr), "listen address (env REGISTRY_ADDR)")
	data := c.String("data", e.envOr("REGISTRY_DATA", defaultDataDir), "directory for the database and stored files (env REGISTRY_DATA)")
	upstream := c.String("upstream", e.envOr("REGISTRY_UPSTREAM", mirror.DefaultEndpoint), "hub to mirror from (env REGISTRY_UPSTREAM)")
	requireAuth := c.Bool("require-auth", c.envBool("REGISTRY_REQUIRE_AUTH"), "require a token for downloads and browsing too (env REGISTRY_REQUIRE_AUTH)")
	requireAdmin := c.Bool("require-admin-token", c.envBool("REGISTRY_REQUIRE_ADMIN_TOKEN"), "refuse to start without a supplied admin token instead of generating one and printing it (env REGISTRY_REQUIRE_ADMIN_TOKEN); use where logs are collected")
	pullThrough := c.Bool("pull-through", c.envBool("REGISTRY_PULL_THROUGH"), "fetch a model from the upstream hub the first time a client asks for one the registry does not hold (env REGISTRY_PULL_THROUGH); limit which with --pull-through-allow")
	allowList := c.String("pull-through-allow", e.getenv("REGISTRY_PULL_THROUGH_ALLOW"), "repositories pull-through may fetch: comma-separated patterns over namespace/name, such as \"meta-llama/*,*/*-GGUF\"; empty allows all (env REGISTRY_PULL_THROUGH_ALLOW)")
	pullMaxFile := c.Int64("pull-through-max-file-bytes", c.envBytes("REGISTRY_PULL_THROUGH_MAX_FILE_BYTES"), "largest file pull-through fetches on demand, in bytes; 0 for no limit (env REGISTRY_PULL_THROUGH_MAX_FILE_BYTES)")
	maxBlob := c.Int64("max-blob-bytes", c.envBytes("REGISTRY_MAX_BLOB_BYTES"), "largest single file an upload may store, in bytes; 0 for no limit (env REGISTRY_MAX_BLOB_BYTES)")
	envEvery, envKeep, err := backupSettings(e)
	c.noteEnv(err)
	backupEvery := c.Duration("backup-interval", envEvery, "take a metadata backup this often, such as 1h; at least a minute; 0 for no scheduled backups (env REGISTRY_BACKUP_INTERVAL)")
	backupKeep := c.Int("backup-keep", envKeep, "how many stored backups to keep; 0 keeps every one (env REGISTRY_BACKUP_KEEP)")
	restoreEmpty := c.Bool("restore-if-empty", c.envBool("REGISTRY_RESTORE_IF_EMPTY"), "when the data directory has no database, start from the newest stored backup (env REGISTRY_RESTORE_IF_EMPTY)")
	storage := c.storageFlags("where model files are kept: filesystem or s3 (env REGISTRY_STORAGE); s3 is configured with the --s3-* flags")
	s3Redirect := c.Bool("s3-redirect", c.envBool("REGISTRY_S3_REDIRECT"), "with s3 storage, answer downloads with a redirect to a signed URL on the object store instead of streaming them through the registry (env REGISTRY_S3_REDIRECT)")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(e.stderr, nil))

	if err := checkBackupSettings(*backupEvery, *backupKeep); err != nil {
		return err
	}
	for name, limit := range map[string]int64{"--pull-through-max-file-bytes": *pullMaxFile, "--max-blob-bytes": *maxBlob} {
		if limit < 0 {
			return usagef("%s must be a whole number of bytes, 0 for no limit; got %d", name, limit)
		}
	}
	allow, err := pullThroughAllow(*allowList)
	if err != nil {
		return err
	}
	// This process owns the data directory from here on, so whatever a
	// previous run left half-received is cleared, once, before anything
	// new is staged. No other command does this.
	if err := datadir.Dir(*data).ClearStaging(); err != nil {
		return err
	}
	store, err := storage.open(e, *data)
	if err != nil {
		return err
	}
	redirect := *s3Redirect && *storage.kind == storageS3
	if *storage.kind == storageS3 {
		log.Info("storing model files in S3", "bucket", *storage.bucket, "endpoint", *storage.endpoint, "prefix", *storage.prefix, "redirect_downloads", redirect)
	}
	if *restoreEmpty {
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
	if *pullThrough && !*requireAuth && len(allow) == 0 {
		// Anyone who can reach the registry can then make it download any
		// public model, which is fine on a private network and not on a public one.
		log.Warn("pull-through is on with open downloads and no allow list: anyone who can reach this registry can make it fetch and store any upstream model; set REGISTRY_REQUIRE_AUTH or REGISTRY_PULL_THROUGH_ALLOW unless the network is private")
	}

	handler := server.New(reg, server.Config{
		AnonymousRead:     !*requireAuth,
		Upstream:          *upstream,
		UpstreamToken:     e.getenv("REGISTRY_UPSTREAM_TOKEN"),
		RedirectDownloads: redirect,
		PullThrough:       *pullThrough,
		PullThroughAllow:  allow,
		BackupKeep:        *backupKeep,
		Version:           Version,
		Logger:            log,

		PullThroughMaxFileBytes: *pullMaxFile,
		MaxBlobBytes:            *maxBlob,
	})
	ln, err := (&net.ListenConfig{}).Listen(e.ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	// No read or write timeout: model files are large and transfers are
	// long. Slow-header attacks are still bounded, and the handlers of the
	// small JSON and form routes set a deadline for their own bodies.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout}
	log.Info("registry listening", "addr", ln.Addr().String(), "data", *data, "upstream", *upstream,
		"anonymous_read", !*requireAuth, "pull_through", *pullThrough, "version", Version)

	var backups func(context.Context)
	if *backupEvery > 0 {
		log.Info("scheduled metadata backups are on", "every", backupEvery.String(), "keep", *backupKeep)
		backups = func(ctx context.Context) {
			ticker := time.NewTicker(*backupEvery)
			defer ticker.Stop()
			runBackups(ctx, reg, ticker.C, *backupKeep, log)
		}
	}
	// The registry is closed (deferred, above) only after this returns.
	return serve(e.ctx, srv, ln, handler, backups, shutdownGrace, log)
}

// Time limits of the HTTP server and of stopping it.
const (
	// readHeaderTimeout is how long a client may take to send a request's
	// headers.
	readHeaderTimeout = 15 * time.Second
	// idleTimeout is how long a connection with no request on it is kept.
	idleTimeout = 2 * time.Minute
	// shutdownGrace is how long stopping waits for transfers in flight and
	// for the server's own work before it gives up on them.
	shutdownGrace = 30 * time.Second
)

// serve answers requests on ln until ctx ends, then stops everything this
// process runs against the registry and returns once all of it has stopped,
// or after grace if some of it has not. Three things are stopped:
//
//   - requests in flight, which the HTTP server waits for;
//   - the server's own pull-through fetches, which no request owns: they
//     are cancelled and waited for (server.Server.Shutdown);
//   - background, the scheduled-backup loop when there is one, which runs
//     on a context that ends here.
//
// The caller closes the registry after serve returns. Returning while one
// of these is still running would let it read and write a closed registry.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, handler *server.Server, background func(context.Context), grace time.Duration, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	backgroundDone := make(chan struct{})
	go func() {
		defer close(backgroundDone)
		if background != nil {
			background(ctx)
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	var failed error
	select {
	case failed = <-serveErr: // the listener failed; stop the rest all the same
	case <-ctx.Done():
		log.Info("shutting down; waiting for transfers in flight")
	}
	stop()

	deadline, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	// Requests and fetches are stopped at the same time, not one after the
	// other: a request can be waiting on a fetch, and is answered as soon
	// as the fetch is cancelled.
	httpStopped := make(chan error, 1)
	go func() { httpStopped <- srv.Shutdown(deadline) }()
	if err := handler.Shutdown(deadline); err != nil {
		log.Warn("stopped waiting for pull-through fetches", "error", err)
	}
	if err := <-httpStopped; err != nil && !errors.Is(err, context.DeadlineExceeded) && failed == nil {
		failed = err
	}
	select {
	case <-backgroundDone:
	case <-deadline.Done():
		log.Warn("stopped waiting for a scheduled backup that was still running")
	}
	return failed
}

// storageFlags are the settings that say where model files are kept. serve
// and restore both take them, since both open the storage.
type storageFlags struct {
	kind, bucket, endpoint, region, prefix *string
}

// storageFlags declares --storage, with the given help text, and the
// --s3-* flags.
func (c *command) storageFlags(kindUsage string) *storageFlags {
	e := c.e
	return &storageFlags{
		kind:     c.String("storage", e.envOr("REGISTRY_STORAGE", storageFilesystem), kindUsage),
		bucket:   c.String("s3-bucket", e.getenv("REGISTRY_S3_BUCKET"), "with s3 storage: the bucket, which must exist (env REGISTRY_S3_BUCKET)"),
		endpoint: c.String("s3-endpoint", e.getenv("REGISTRY_S3_ENDPOINT"), "with s3 storage: the service's URL; empty for AWS (env REGISTRY_S3_ENDPOINT)"),
		region:   c.String("s3-region", e.envOr("REGISTRY_S3_REGION", e.getenv("AWS_REGION")), "with s3 storage: the region (env REGISTRY_S3_REGION, then AWS_REGION)"),
		prefix:   c.String("s3-prefix", e.getenv("REGISTRY_S3_PREFIX"), "with s3 storage: a prefix for every object key, to share a bucket (env REGISTRY_S3_PREFIX)"),
	}
}

// open builds the storage backend the settings name. The S3 keys are the
// part of its configuration that has no flag.
func (s *storageFlags) open(e *env, data string) (blob.Store, error) {
	switch *s.kind {
	case storageFilesystem:
		return blob.NewFS(data)
	case storageS3:
		if *s.bucket == "" {
			return nil, usagef("--storage s3 needs a bucket: set --s3-bucket or REGISTRY_S3_BUCKET")
		}
		return blob.NewS3(e.ctx, blob.S3Config{
			Endpoint:     *s.endpoint,
			Region:       *s.region,
			Bucket:       *s.bucket,
			Prefix:       *s.prefix,
			AccessKey:    e.envOr("REGISTRY_S3_ACCESS_KEY", e.getenv("AWS_ACCESS_KEY_ID")),
			SecretKey:    e.envOr("REGISTRY_S3_SECRET_KEY", e.getenv("AWS_SECRET_ACCESS_KEY")),
			SessionToken: e.getenv("AWS_SESSION_TOKEN"),
			StagingDir:   datadir.Dir(data).Staging(),
		})
	}
	return nil, usagef("unknown --storage %q: use %s or %s", *s.kind, storageFilesystem, storageS3)
}

// pullThroughAllow reads the allow list: patterns separated by commas.
func pullThroughAllow(list string) ([]string, error) {
	var allow []string
	for _, pattern := range strings.Split(list, ",") {
		if pattern = strings.TrimSpace(pattern); pattern == "" {
			continue
		}
		if _, err := path.Match(pattern, "a/b"); err != nil {
			return nil, usagef("REGISTRY_PULL_THROUGH_ALLOW (--pull-through-allow): %q is not a valid pattern", pattern)
		}
		allow = append(allow, pattern)
	}
	return allow, nil
}

// backupSettings reads the schedule for metadata backups from the
// environment: how often, and how many to keep.
func backupSettings(e *env) (every time.Duration, keep int, err error) {
	keep = defaultBackupKeep
	if v := e.getenv("REGISTRY_BACKUP_KEEP"); v != "" {
		keep, err = strconv.Atoi(v)
		if err != nil || keep < 0 {
			return 0, 0, usagef("REGISTRY_BACKUP_KEEP must be a number, 0 to keep every backup; got %q", v)
		}
	}
	if v := e.getenv("REGISTRY_BACKUP_INTERVAL"); v != "" {
		every, err = time.ParseDuration(v)
		if err != nil || every < minBackupInterval {
			return 0, 0, usagef("REGISTRY_BACKUP_INTERVAL must be a duration of at least a minute, such as 1h or 30m; got %q", v)
		}
	}
	return every, keep, nil
}

// checkBackupSettings holds the schedule given by flags to the rules
// backupSettings holds the environment to. An interval of 0 means no
// scheduled backups.
func checkBackupSettings(every time.Duration, keep int) error {
	if keep < 0 {
		return usagef("--backup-keep must be a number, 0 to keep every backup; got %d", keep)
	}
	if every != 0 && every < minBackupInterval {
		return usagef("--backup-interval must be a duration of at least a minute, such as 1h or 30m; got %s", every)
	}
	return nil
}

// runBackups takes a metadata backup each time tick delivers, until ctx
// ends. The caller owns the ticker, so a test can send the ticks itself. A
// failed backup is logged and the schedule carries on.
func runBackups(ctx context.Context, reg *registry.Registry, tick <-chan time.Time, keep int, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
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

// noteEnv remembers the first environment variable whose value could not
// be read, for parse to report.
func (c *command) noteEnv(err error) {
	if err != nil && c.envErr == nil {
		c.envErr = err
	}
}

// envBool reads an on/off variable to use as its flag's default. A value
// that cannot be read is noted, and reported once the flags are parsed.
func (c *command) envBool(key string) bool {
	b, err := envBool(c.e, key)
	c.noteEnv(err)
	return b
}

// envBytes reads a size limit to use as its flag's default, likewise.
func (c *command) envBytes(key string) int64 {
	n, err := envBytes(c.e, key)
	c.noteEnv(err)
	return n
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

// envBool reads an on/off setting; unset means off. It accepts what
// strconv.ParseBool does (1, t, true, 0, f, false, in its spellings) and
// yes, no, on and off in any case. Anything else is an error: a setting
// such as REGISTRY_REQUIRE_AUTH must not be quietly off because it was
// written in a way this does not know.
func envBool(e *env, key string) (bool, error) {
	v := strings.TrimSpace(e.getenv(key))
	if v == "" {
		return false, nil
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b, nil
	}
	switch strings.ToLower(v) {
	case "yes", "on":
		return true, nil
	case "no", "off":
		return false, nil
	}
	return false, usagef("%s must be true or false (1, t, yes and on are also read as true; 0, f, no and off as false); got %q", key, v)
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
