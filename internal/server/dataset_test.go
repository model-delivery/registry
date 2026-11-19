// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// datasetFiles is a dataset of the same name as the model of modelFiles,
// with other content and one file in common.
var datasetFiles = map[string]string{
	"README.md":           "# a dataset card",
	"data/train.csv":      "text,label\nhello,1\nbye,0\n",
	"data/test.csv":       "text,label\nyes,1\n",
	"tokenizer/vocab.txt": "a\nb\nc\n", // the same content as the model's
}

// withBoth is a registry holding the model acme/tiny and the dataset
// acme/tiny.
func withBoth(t *testing.T, cfg server.Config) (*regtest.Instance, client.PushResult, client.PushResult) {
	t.Helper()
	in := regtest.Start(t, cfg)
	m := push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	d := push(t, in, "datasets/acme/tiny", datasetFiles, client.PushOptions{})
	return in, m, d
}

// The Hugging Face routes serve a dataset at the Hub's dataset addresses,
// and a model and a dataset of the same name never see each other's files.
func TestDatasetHubRoutes(t *testing.T) {
	t.Parallel()
	in, m, d := withBoth(t, server.Config{AnonymousRead: true})
	if m.SHA == d.SHA || d.RepoType != client.Dataset || m.RepoType != "" {
		t.Fatalf("pushed %+v and %+v", m, d)
	}

	t.Run("dataset_info", func(t *testing.T) {
		t.Parallel()
		var doc map[string]any
		resp := do(t, http.MethodGet, in.URL+"/api/datasets/acme/tiny/revision/main", "", nil, nil)
		decode(t, resp, &doc)
		if resp.StatusCode != http.StatusOK || doc["id"] != "acme/tiny" || doc["sha"] != d.SHA {
			t.Fatalf("status %d, %v", resp.StatusCode, doc)
		}
		if _, ok := doc["modelId"]; ok {
			t.Errorf("a dataset's description has modelId: %v", doc)
		}
		if siblings, _ := doc["siblings"].([]any); len(siblings) != len(datasetFiles) {
			t.Errorf("siblings %v", doc["siblings"])
		}
		var model map[string]any
		decode(t, do(t, http.MethodGet, in.URL+"/api/models/acme/tiny", "", nil, nil), &model)
		if model["sha"] != m.SHA || model["modelId"] != "acme/tiny" {
			t.Errorf("the model of the same name: %v", model)
		}
	})

	t.Run("tree and refs", func(t *testing.T) {
		t.Parallel()
		var tree []map[string]any
		decode(t, do(t, http.MethodGet, in.URL+"/api/datasets/acme/tiny/tree/main/data", "", nil, nil), &tree)
		if len(tree) != 2 || tree[0]["path"] != "data/test.csv" || tree[1]["path"] != "data/train.csv" {
			t.Fatalf("tree %v", tree)
		}
		var refs map[string][]map[string]any
		decode(t, do(t, http.MethodGet, in.URL+"/api/datasets/acme/tiny/refs", "", nil, nil), &refs)
		if len(refs["branches"]) != 1 || refs["branches"][0]["targetCommit"] != d.SHA {
			t.Fatalf("refs %v", refs)
		}
	})

	t.Run("files", func(t *testing.T) {
		t.Parallel()
		resp := do(t, http.MethodGet, in.URL+"/datasets/acme/tiny/resolve/main/data/train.csv", "", nil, nil)
		if got := body(t, resp); resp.StatusCode != http.StatusOK || got != datasetFiles["data/train.csv"] || resp.Header.Get("X-Repo-Commit") != d.SHA {
			t.Fatalf("status %d, commit %q, %q", resp.StatusCode, resp.Header.Get("X-Repo-Commit"), got)
		}
		// The model of the same name has no such file, and the dataset has
		// none of the model's.
		for path, want := range map[string]string{
			"/acme/tiny/resolve/main/data/train.csv":             "EntryNotFound",
			"/datasets/acme/tiny/resolve/main/model.safetensors": "EntryNotFound",
			"/datasets/acme/other/resolve/main/README.md":        "RepoNotFound",
		} {
			resp := do(t, http.MethodHead, in.URL+path, "", nil, nil)
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != want {
				t.Errorf("%s: status %d, code %q; want 404 %s", path, resp.StatusCode, resp.Header.Get("X-Error-Code"), want)
			}
		}
	})

	t.Run("paths-info", func(t *testing.T) {
		t.Parallel()
		form := url.Values{"paths": {"data/train.csv", "data", "missing.txt", "README.md"}, "expand": {"false"}}
		for repo, want := range map[string]string{
			"/api/datasets/acme/tiny": "data/train.csv file,data directory,README.md file",
			"/api/models/acme/tiny":   "",
		} {
			resp := do(t, http.MethodPost, in.URL+repo+"/paths-info/main", "", strings.NewReader(form.Encode()),
				map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
			var entries []map[string]any
			decode(t, resp, &entries)
			var got []string
			for _, e := range entries {
				got = append(got, e["path"].(string)+" "+e["type"].(string))
			}
			if resp.StatusCode != http.StatusOK || strings.Join(got, ",") != want {
				t.Errorf("%s: status %d, %v; want %q", repo, resp.StatusCode, got, want)
			}
		}
		// And for a model, its own paths.
		modelForm := url.Values{"paths": {"config.json", "tokenizer"}}
		resp := do(t, http.MethodPost, in.URL+"/api/models/acme/tiny/paths-info/main", "", strings.NewReader(modelForm.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		var entries []map[string]any
		decode(t, resp, &entries)
		if len(entries) != 2 || entries[0]["path"] != "config.json" || entries[0]["size"] != float64(len(modelFiles["config.json"])) ||
			entries[1]["type"] != "directory" {
			t.Errorf("the model's paths-info: %v", entries)
		}
	})

	// dataset_info and model_info with files_metadata=True ask for blobs=true
	// and read each file's size, blob id and LFS digest from the siblings;
	// huggingface.co gives them, and the tree gives the same values.
	t.Run("info with blobs=true", func(t *testing.T) {
		t.Parallel()
		type sibling struct {
			RFilename string `json:"rfilename"`
			BlobID    string `json:"blobId"`
			Size      *int64 `json:"size"`
			LFS       *struct {
				SHA256      string `json:"sha256"`
				Size        int64  `json:"size"`
				PointerSize int    `json:"pointerSize"`
			} `json:"lfs"`
		}
		for api, files := range map[string]map[string]string{"datasets": datasetFiles, "models": modelFiles} {
			var tree []struct {
				Type string `json:"type"`
				OID  string `json:"oid"`
				Size int64  `json:"size"`
				Path string `json:"path"`
				LFS  *struct {
					OID string `json:"oid"`
				} `json:"lfs"`
			}
			decode(t, do(t, http.MethodGet, in.URL+"/api/"+api+"/acme/tiny/tree/main?recursive=true", "", nil, nil), &tree)
			var doc struct {
				Siblings []sibling `json:"siblings"`
			}
			decode(t, do(t, http.MethodGet, in.URL+"/api/"+api+"/acme/tiny/revision/main?blobs=true", "", nil, nil), &doc)
			if len(doc.Siblings) != len(files) {
				t.Fatalf("%s: siblings %+v", api, doc.Siblings)
			}
			for _, s := range doc.Siblings {
				var entry = -1
				for i, e := range tree {
					if e.Path == s.RFilename {
						entry = i
					}
				}
				switch {
				case entry < 0:
					t.Errorf("%s: %s is a sibling but not in the tree", api, s.RFilename)
				case s.Size == nil || *s.Size != int64(len(files[s.RFilename])):
					t.Errorf("%s: %s has size %v, want %d", api, s.RFilename, s.Size, len(files[s.RFilename]))
				case s.BlobID == "" || s.BlobID != tree[entry].OID:
					t.Errorf("%s: %s has blobId %q, the tree says %q", api, s.RFilename, s.BlobID, tree[entry].OID)
				case (s.LFS == nil) != (tree[entry].LFS == nil):
					t.Errorf("%s: %s: lfs %+v in the siblings, %+v in the tree", api, s.RFilename, s.LFS, tree[entry].LFS)
				case s.LFS != nil && (s.LFS.SHA256 != tree[entry].LFS.OID || s.LFS.Size != *s.Size || s.LFS.PointerSize == 0):
					t.Errorf("%s: %s: lfs %+v, the tree's digest %s", api, s.RFilename, s.LFS, tree[entry].LFS.OID)
				}
			}
			// Without blobs, the name alone, as the Hub answers.
			var plain struct {
				Siblings []map[string]any `json:"siblings"`
			}
			decode(t, do(t, http.MethodGet, in.URL+"/api/"+api+"/acme/tiny/revision/main", "", nil, nil), &plain)
			for _, s := range plain.Siblings {
				if len(s) != 1 || s["rfilename"] == nil {
					t.Errorf("%s: without blobs=true, sibling %v", api, s)
				}
			}
		}
	})

	// The dataset viewer is not implemented. What the datasets library asks
	// of it is answered 404, which is what makes it read the repository's
	// own files.
	t.Run("the viewer service is not implemented", func(t *testing.T) {
		t.Parallel()
		for _, path := range []string{
			"/api/datasets/acme/tiny/parquet",
			"/api/datasets/acme/tiny/parquet/default/train",
			"/api/datasets/acme/tiny/croissant",
			"/info?dataset=acme/tiny",
			"/parquet?dataset=acme/tiny",
		} {
			resp := do(t, http.MethodGet, in.URL+path, "", nil, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
			}
		}
	})

	t.Run("spaces are not supported", func(t *testing.T) {
		t.Parallel()
		resp := do(t, http.MethodGet, in.URL+"/api/spaces/acme/tiny", "", nil, nil)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body(t, resp), "not implemented") {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

// The registry's API keeps models under /api/v1/repos, exactly as before,
// and serves datasets under /api/v1/datasets.
func TestDatasetRegistryAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, m, d := withBoth(t, server.Config{AnonymousRead: true})
	cl := client.New(in.URL, in.Admin)

	models, err := cl.ListRepositories(ctx, client.Model)
	if err != nil || len(models.Repos) != 1 || models.Repos[0].Type != "" || strings.Contains(string(models.Raw), `"type"`) {
		t.Fatalf("models: %s, %v", models.Raw, err)
	}
	datasets, err := cl.ListRepositories(ctx, client.Dataset)
	if err != nil || len(datasets.Repos) != 1 || datasets.Repos[0].Type != client.Dataset {
		t.Fatalf("datasets: %s, %v", datasets.Raw, err)
	}
	rev, err := cl.Revision(ctx, "datasets/acme/tiny", "main")
	if err != nil || rev.SHA != d.SHA || rev.RepoType != client.Dataset || len(rev.Files) != len(datasetFiles) {
		t.Fatalf("the dataset's revision: %+v, %v", rev, err)
	}
	detail, err := cl.Repo(ctx, "datasets/acme/tiny")
	if err != nil || detail.Type != client.Dataset || len(detail.Revisions) != 1 {
		t.Fatalf("the dataset: %+v, %v", detail, err)
	}

	// Deleting the dataset frees what only it held and leaves the model,
	// and the file they share, alone.
	freed, err := cl.DeleteRepo(ctx, "datasets/acme/tiny")
	if err != nil || freed.Blobs != len(datasetFiles)-1 {
		t.Fatalf("delete the dataset: %+v, %v", freed, err)
	}
	if _, err := cl.Repo(ctx, "datasets/acme/tiny"); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("the dataset after deletion: %v", err)
	}
	model, err := cl.Revision(ctx, "acme/tiny", "main")
	if err != nil || model.SHA != m.SHA || len(model.Files) != len(modelFiles) {
		t.Fatalf("the model after the dataset was deleted: %+v, %v", model, err)
	}
	if _, err := cl.Pull(ctx, "acme/tiny", "main", t.TempDir(), nil); err != nil {
		t.Fatalf("the model's files after the dataset was deleted: %v", err)
	}
}

func isStatus(err error, status int) bool {
	var apiErr *client.APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// An upload from a Hugging Face client to a dataset records a revision of
// the dataset, through the same steps as a model's.
func TestDatasetHubUpload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	auth := map[string]string{"Content-Type": "application/json"}

	resp := do(t, http.MethodPost, in.URL+"/api/datasets/acme/up/preupload/main", in.Write,
		strings.NewReader(`{"files":[{"path":"a.csv","size":4}]}`), auth)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(got, `"uploadMode":"regular"`) {
		t.Fatalf("preupload: %d %s", resp.StatusCode, got)
	}
	resp = do(t, http.MethodPost, in.URL+"/datasets/acme/up.git/info/lfs/objects/batch", in.Write,
		strings.NewReader(`{"operation":"upload","objects":[{"oid":"`+strings.Repeat("a", 64)+`","size":5}]}`), auth)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(got, "/api/lfs/upload/") {
		t.Fatalf("large-file batch: %d %s", resp.StatusCode, got)
	}
	commit := `{"key":"header","value":{"summary":"add data"}}` + "\n" +
		`{"key":"file","value":{"path":"a.csv","content":"` + base64.StdEncoding.EncodeToString([]byte("x,y\n")) + `","encoding":"base64"}}` + "\n"
	resp = do(t, http.MethodPost, in.URL+"/api/datasets/acme/up/commit/main", in.Write, strings.NewReader(commit),
		map[string]string{"Content-Type": "application/x-ndjson"})
	if got := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(got, "/datasets/acme/up/commit/") {
		t.Fatalf("commit: %d %s", resp.StatusCode, got)
	}
	c, err := in.Reg.Resolve(ctx, "datasets/acme/up", "main")
	if err != nil || c.RepoType != registry.TypeDataset || c.Message != "add data" {
		t.Fatalf("the dataset's revision: %+v, %v", c, err)
	}
	if _, err := in.Reg.GetRepo(ctx, "acme/up"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("a model was created: %v", err)
	}
	// A branch of the dataset, and a xet token request answered as a
	// model's is.
	if resp := do(t, http.MethodPost, in.URL+"/api/datasets/acme/up/branch/dev", in.Write, strings.NewReader(`{}`), auth); resp.StatusCode != http.StatusOK {
		t.Fatalf("create branch: %d %s", resp.StatusCode, body(t, resp))
	}
	if _, err := in.Reg.Resolve(ctx, "datasets/acme/up", "dev"); err != nil {
		t.Fatalf("the branch: %v", err)
	}
	if resp := do(t, http.MethodGet, in.URL+"/api/datasets/acme/up/xet-write-token/main", in.Write, nil, nil); resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("xet token: %d", resp.StatusCode)
	}
}

// The web UI lists models and datasets, tells them apart, and shows and
// deletes each at its own address.
func TestDatasetUI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, _, d := withBoth(t, server.Config{AnonymousRead: true})

	home := body(t, do(t, http.MethodGet, in.URL+"/", "", nil, nil))
	for _, want := range []string{`href="/ui/repo/acme/tiny"`, `href="/ui/dataset/acme/tiny"`, `<span class="tag">dataset</span>`} {
		if !strings.Contains(home, want) {
			t.Errorf("the home page lacks %s", want)
		}
	}
	page := body(t, do(t, http.MethodGet, in.URL+"/ui/dataset/acme/tiny", in.Admin, nil, nil)) // the delete form is shown to admins
	for _, want := range []string{"--repo-type dataset --revision " + d.SHA, `href="/datasets/acme/tiny/resolve/` + d.SHA + `/data/train.csv"`, `action="/ui/dataset/acme/tiny/delete"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the dataset's page lacks %s", want)
		}
	}
	if model := body(t, do(t, http.MethodGet, in.URL+"/ui/repo/acme/tiny", "", nil, nil)); strings.Contains(model, "repo-type") || strings.Contains(model, "train.csv") {
		t.Error("the model's page shows the dataset")
	}
	if resp := do(t, http.MethodGet, in.URL+"/datasets/acme/tiny", "", nil, nil); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/dataset/acme/tiny" {
		t.Errorf("hub-style dataset link: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	del := func(confirm string) *http.Response {
		return do(t, http.MethodPost, in.URL+"/ui/dataset/acme/tiny/delete", in.Admin, strings.NewReader(url.Values{"confirm": {confirm}}.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": in.URL})
	}
	if resp := del("datasets/acme/tiny"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/ui/dataset/acme/tiny") {
		t.Fatalf("delete with the wrong name: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := del("acme/tiny"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") == "" || strings.Contains(resp.Header.Get("Location"), "/ui/") {
		t.Fatalf("delete: %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, err := in.Reg.GetRepo(ctx, "datasets/acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("the dataset is still there: %v", err)
	}
	if _, err := in.Reg.GetRepo(ctx, "acme/tiny"); err != nil {
		t.Fatalf("deleting the dataset deleted the model: %v", err)
	}
}

// A dataset is mirrored from the upstream's dataset addresses, recorded as
// a dataset, and served without the upstream.
func TestMirrorADatasetThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub, _, d := withBoth(t, server.Config{AnonymousRead: true})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.URL})
	cl := client.New(local.URL, local.Write)

	var start client.MirrorEvent
	done, err := cl.Mirror(ctx, "datasets/acme/tiny", "main", "", func(e client.MirrorEvent) {
		if e.Event == client.EventStart {
			start = e
		}
	})
	if err != nil || done.SHA != d.SHA || done.Repo != "acme/tiny" || done.RepoType != client.Dataset || start.RepoType != client.Dataset {
		t.Fatalf("mirror: start %+v, done %+v, %v", start, done, err)
	}
	c, err := local.Reg.Resolve(ctx, "datasets/acme/tiny", "main")
	if err != nil || c.RepoType != registry.TypeDataset {
		t.Fatalf("recorded %+v, %v", c, err)
	}
	if _, err := local.Reg.GetRepo(ctx, "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("mirroring the dataset recorded the model too: %v", err)
	}
	hub.StopServing()
	resp := do(t, http.MethodGet, local.URL+"/datasets/acme/tiny/resolve/main/data/train.csv", "", nil, nil)
	if got := body(t, resp); resp.StatusCode != http.StatusOK || got != datasetFiles["data/train.csv"] {
		t.Fatalf("served without the upstream: %d %q", resp.StatusCode, got)
	}

	// What the request may name.
	for name, payload := range map[string]string{
		"a space":                         `{"repo":"acme/tiny","type":"space"}`,
		"a name of three segments":        `{"repo":"acme/tiny/x","type":"dataset"}`,
		"a key passed off as a name":      `{"repo":"datasets/acme/tiny"}`,
		"a type in the wrong case":        `{"repo":"acme/tiny","type":"Dataset"}`,
		"the reserved datasets namespace": `{"repo":"datasets/tiny","type":"dataset"}`,
	} {
		resp := do(t, http.MethodPost, local.URL+"/api/v1/mirror", local.Write, strings.NewReader(payload), map[string]string{"Content-Type": "application/json"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d (%s), want 400", name, resp.StatusCode, body(t, resp))
		}
	}
}
