package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jangjaehong/file-server/pkg/publish"
)

func TestPublishAutoCreateAndIdempotent(t *testing.T) {
	e := setup(t)
	e.req("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "password123"}, 200)
	token := e.req("POST", "/api/v1/admin/tokens", map[string]string{"name": "ci"}, 201)["secret"].(string)

	dir := t.TempDir()
	exe := filepath.Join(dir, "ics-scm.exe")
	os.WriteFile(exe, []byte("build 57"), 0o644)
	os.WriteFile(filepath.Join(dir, "version.txt"), []byte("\ufeff1.0.0.57\r\n"), 0o644)
	if v := publish.VersionFromFile(exe); v != "1.0.0.57" {
		t.Fatalf("VersionFromFile = %q", v)
	}
	opts := func() publish.Options {
		return publish.Options{Server: e.ts.URL, Token: token, Program: "ics-scm", Version: "1.0.0.57",
			Notes: "first", Files: map[string]string{"windows-amd64": exe}}
	}
	ctx := context.Background()

	// First deployment: program does not exist yet -> created with slug as name.
	res, err := publish.Run(ctx, opts())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProgramCreated || !res.Published || res.LatestJSON != e.ts.URL+"/ics-scm/latest.json" {
		t.Fatalf("first run: %+v", res)
	}
	p := e.req("GET", "/api/v1/programs/ics-scm", nil, 200)
	if p["name"] != "ics-scm" || p["latest"].(map[string]any)["version"] != "1.0.0.57" {
		t.Fatalf("program after first deploy: %v", p)
	}

	// CI re-run with the identical file: success, nothing changed.
	res, err = publish.Run(ctx, opts())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProgramCreated || !res.AlreadyPublished || res.Published || len(res.Uploaded) != 0 {
		t.Fatalf("re-run: %+v", res)
	}

	// Same version, different bytes: refused (published versions are immutable).
	os.WriteFile(exe, []byte("different build"), 0o644)
	if _, err := publish.Run(ctx, opts()); !errors.Is(err, publish.ErrImmutable) {
		t.Fatalf("expected ErrImmutable, got %v", err)
	}

	// Next build: update is published automatically and becomes latest.json.
	o := opts()
	o.Version = "1.0.0.58"
	o.Notes = ""
	if _, err := publish.Run(ctx, o); err != nil {
		t.Fatal(err)
	}
	if code, raw := get(t, e.ts.URL+"/ics-scm/latest.json"); code != 200 || !strings.Contains(string(raw), `"1.0.0.58"`) {
		t.Fatalf("latest.json after update: %d %s", code, raw)
	}

	// Interrupted run left a draft with an old file: resumed, file replaced, published.
	o.Version = "1.0.0.59"
	o.Draft = true
	o.Notes = "draft notes"
	if res, err := publish.Run(ctx, o); err != nil || res.Published {
		t.Fatalf("draft run: %+v %v", res, err)
	}
	os.WriteFile(exe, []byte("build 59 rebuilt"), 0o644)
	o.Draft = false
	o.Notes = "" // keeps notes already stored on the draft
	res, err = publish.Run(ctx, o)
	if err != nil || !res.Published || len(res.Uploaded) != 1 {
		t.Fatalf("resume: %+v %v", res, err)
	}
	rel := e.req("GET", "/api/v1/admin/programs/ics-scm/releases/1.0.0.59", nil, 200)
	if rel["notes"] != "draft notes" || len(rel["assets"].([]any)) != 1 {
		t.Fatalf("resumed release: %v", rel)
	}
	if _, raw := get(t, e.ts.URL+"/ics-scm/ics-scm-1.0.0.59.exe"); string(raw) != "build 59 rebuilt" {
		t.Fatalf("served %q", raw)
	}

	// Fixed file only (defaults.json), program already exists.
	def := filepath.Join(dir, "defaults.json")
	os.WriteFile(def, []byte(`{"x":1}`), 0o644)
	if _, err := publish.Run(ctx, publish.Options{Server: e.ts.URL, Token: token, Program: "ics-scm", Fixed: []string{def}}); err != nil {
		t.Fatal(err)
	}
	if _, raw := get(t, e.ts.URL+"/ics-scm/defaults.json"); string(raw) != `{"x":1}` {
		t.Fatalf("defaults.json %q", raw)
	}

	// Bad token is reported, not silently skipped.
	o.Token = "fst_wrong"
	o.Version = "1.0.0.60"
	if _, err := publish.Run(ctx, o); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}
}
