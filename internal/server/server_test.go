package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/jangjaehong/file-server/internal/store"
	"github.com/jangjaehong/file-server/pkg/updater"
)

type env struct {
	t   *testing.T
	ts  *httptest.Server
	hc  *http.Client
	dir string
}

func setup(t *testing.T) *env {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAdmin(context.Background(), "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{DataDir: dir}, st, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, ts: ts, hc: &http.Client{Jar: jar}, dir: dir}
}

func (e *env) req(method, path string, body any, want int) map[string]any {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "test")
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: got %d want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (e *env) upload(version, platform, name string, data []byte, want int) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("platform", platform)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/admin/programs/app-a/releases/"+version+"/assets", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "test")
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		e.t.Fatalf("upload %s %s: got %d want %d: %s", version, name, resp.StatusCode, want, raw)
	}
}

func TestFullFlow(t *testing.T) {
	e := setup(t)

	// Admin endpoints require login.
	e.req("POST", "/api/v1/admin/programs", map[string]string{"slug": "app-a", "name": "A"}, 401)
	e.req("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "wrong-pass"}, 401)
	e.req("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "password123"}, 200)

	e.req("POST", "/api/v1/admin/programs", map[string]string{"slug": "app-a", "name": "Program A", "description": "# A"}, 201)
	e.req("POST", "/api/v1/admin/programs", map[string]string{"slug": "Bad Slug", "name": "x"}, 400)

	// v1.0.0
	e.req("POST", "/api/v1/admin/programs/app-a/releases", map[string]any{"version": "1.0.0", "notes": "first"}, 201)
	e.req("POST", "/api/v1/admin/programs/app-a/releases", map[string]any{"version": "v1.0.0"}, 409) // immutable
	e.req("POST", "/api/v1/admin/programs/app-a/releases/1.0.0/publish", nil, 409)                   // no files yet
	e.upload("1.0.0", "windows-amd64", "a.exe", []byte("v1-win"), 201)
	e.upload("1.0.0", "windows-amd64", "a.exe", []byte("dup"), 409)
	e.upload("1.0.0", "linux-amd64", "../../etc/a", []byte("v1-linux"), 201) // name sanitized to "a"
	e.req("POST", "/api/v1/admin/programs/app-a/releases/1.0.0/publish", nil, 200)
	e.upload("1.0.0", "linux-amd64", "b", []byte("late"), 409) // published => immutable

	// v1.1.0 mandatory, v1.2.0 normal, 1.3.0-beta.1 beta, 1.2.1 draft (invisible)
	for _, r := range []map[string]any{
		{"version": "1.1.0", "notes": "security fix", "mandatory": true},
		{"version": "1.2.0", "notes": "features"},
		{"version": "1.3.0-beta.1", "notes": "beta", "channel": "beta"},
		{"version": "1.2.1", "notes": "wip"},
	} {
		v := r["version"].(string)
		e.req("POST", "/api/v1/admin/programs/app-a/releases", r, 201)
		e.upload(v, "windows-amd64", "a.exe", []byte("win-"+v), 201)
		if v != "1.2.1" {
			e.req("POST", "/api/v1/admin/programs/app-a/releases/"+v+"/publish", nil, 200)
		}
	}

	// Public view hides drafts.
	anon := &env{t: t, ts: e.ts, hc: http.DefaultClient}
	p := anon.req("GET", "/api/v1/programs/app-a", nil, 200)
	if p["latest"].(map[string]any)["version"] != "1.2.0" {
		t.Fatalf("latest = %v", p["latest"])
	}
	if p["latest_beta"].(map[string]any)["version"] != "1.3.0-beta.1" {
		t.Fatalf("latest_beta = %v", p["latest_beta"])
	}
	if n := len(p["releases"].([]any)); n != 4 {
		t.Fatalf("public releases = %d, want 4", n)
	}
	anon.req("GET", "/download/app-a/1.2.1/windows-amd64", nil, 404)

	// Update check through the client library.
	c := updater.New(e.ts.URL, "app-a")
	c.Platform = "windows-amd64"
	info, err := c.Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdateAvailable || info.Latest.Version != "1.2.0" || !info.Mandatory || len(info.Changes) != 2 {
		t.Fatalf("unexpected update info: %+v", info)
	}
	info2, _ := c.Check(context.Background(), "1.1.0")
	if info2.Mandatory || len(info2.Changes) != 1 {
		t.Fatalf("from 1.1.0: %+v", info2)
	}
	c.Channel = "beta"
	info3, _ := c.Check(context.Background(), "1.2.0")
	if !info3.UpdateAvailable || info3.Latest.Version != "1.3.0-beta.1" {
		t.Fatalf("beta: %+v", info3)
	}
	c.Channel = "stable"
	if i, _ := c.Check(context.Background(), "1.2.0"); i.UpdateAvailable {
		t.Fatal("1.2.0 should be up to date on stable")
	}
	// linux only exists in 1.0.0 => no newer linux build
	c.Platform = "linux-amd64"
	if i, _ := c.Check(context.Background(), "1.0.0"); i.UpdateAvailable {
		t.Fatalf("linux: %+v", i)
	}
	c.Platform = "windows-amd64"

	// Download + checksum verification + replace.
	path, err := c.Download(context.Background(), info, filepath.Join(e.dir, "dl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "win-1.2.0" {
		t.Fatalf("downloaded %q", got)
	}
	target := filepath.Join(e.dir, "installed.exe")
	os.WriteFile(target, []byte("old"), 0o755)
	if err := updater.ReplaceFile(target, path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "win-1.2.0" {
		t.Fatalf("replaced content %q", b)
	}
	if b, _ := os.ReadFile(target + ".old"); string(b) != "old" {
		t.Fatalf("backup content %q", b)
	}

	// Stable "latest" links.
	resp, _ := http.Get(e.ts.URL + "/download/app-a/latest/windows-amd64")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sum := sha256.Sum256(b)
	if string(b) != "win-1.2.0" || resp.Header.Get("X-Checksum-SHA256") != hex.EncodeToString(sum[:]) {
		t.Fatalf("latest download = %q", b)
	}

	// Yank 1.2.0: clients on 1.2.0 must move (to 1.1.0 = newest non-yanked).
	e.req("POST", "/api/v1/admin/programs/app-a/releases/1.2.0/yank", nil, 200)
	i4, _ := c.Check(context.Background(), "1.2.0")
	if !i4.CurrentYanked || !i4.UpdateAvailable || i4.Latest.Version != "1.1.0" || !i4.Mandatory {
		t.Fatalf("after yank: %+v", i4)
	}
	e.req("DELETE", "/api/v1/admin/programs/app-a/releases/1.1.0", nil, 409) // only drafts deletable
	e.req("DELETE", "/api/v1/admin/programs/app-a/releases/1.2.1", nil, 200)

	// API token flow.
	tok := e.req("POST", "/api/v1/admin/tokens", map[string]string{"name": "ci"}, 201)["secret"].(string)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/admin/programs/app-a/releases", bytes.NewReader([]byte(`{"version":"2.0.0"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("token create release: %v %v", err, resp.Status)
	}
	resp.Body.Close()

	// Logout invalidates the session.
	e.req("POST", "/api/v1/auth/logout", nil, 200)
	e.req("GET", "/api/v1/admin/programs", nil, 401)
}

func TestCSRFHeaderRequired(t *testing.T) {
	e := setup(t)
	e.req("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "password123"}, 200)
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/admin/programs", bytes.NewReader([]byte(`{"slug":"x","name":"x"}`)))
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("got %d, want 403", resp.StatusCode)
	}
}
