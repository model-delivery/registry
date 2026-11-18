// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/model-delivery/registry/internal/client"
)

// modelctl verify asks a running registry to re-read every stored file and
// check it against its digest. It exits non-zero when any file fails.

func cmdVerify(e *env, args []string) error {
	c := newClientCommand(e, "verify", "verify [flags]")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	var failed []client.VerifyEvent
	done, err := c.client().Verify(e.ctx, func(ev client.VerifyEvent) {
		switch {
		case ev.Event == "start" && !c.jsonOut:
			fmt.Fprintf(e.stderr, "Checking %d stored files, %s. This reads every one of them in full.\n", ev.Blobs, humanBytes(ev.Bytes))
		case ev.Event == "blob" && ev.Status != "ok":
			failed = append(failed, ev)
			if c.jsonOut {
				return
			}
			used := "not used by any revision"
			if len(ev.Repos) > 0 {
				used = "used by " + strings.Join(ev.Repos, ", ")
			}
			fmt.Fprintf(e.stdout, "%-10s  %s  %s\n            %s\n            %s\n", ev.Status, ev.SHA256, humanBytes(ev.Size), ev.Detail, used)
		}
	})
	if err != nil {
		return err
	}
	if c.jsonOut {
		if failed == nil {
			failed = []client.VerifyEvent{}
		}
		if err := c.printJSON(map[string]any{"blobs": done.Blobs, "bytes": done.Bytes, "failed": done.Failed, "problems": failed}); err != nil {
			return err
		}
	}
	if done.Failed > 0 {
		return fmt.Errorf("%d of %d stored files failed the check. Nothing was changed. "+
			"docs/operations.md, \"Checking stored files\", says how to put each kind right", done.Failed, done.Blobs)
	}
	if !c.jsonOut {
		fmt.Fprintf(e.stdout, "Verified %d stored files (%s): every one is present and matches its digest\n", done.Blobs, humanBytes(done.Bytes))
	}
	return nil
}
