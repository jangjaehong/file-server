package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"testing"
)

func (e *env) uploadTo(path, field, platform, name string, data []byte, want int) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if platform != "" {
		mw.WriteField("platform", platform)
	}
	fw, _ := mw.CreateFormFile(field, name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.ts.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "test")
	resp, err := e.hc.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		e.t.Fatalf("upload %s: got %d want %d: %s", path, resp.StatusCode, want, raw)
	}
}

func get(t *testing.T, u string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// Mimics One TaskBar / ICS-SCM: read <server>/onetaskbar/latest.json, resolve "file"
// relative to it, download and verify SHA-256.
func TestStaticLatestJSONLayout(t *testing.T) {
	e := setup(t)
	e.req("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": "password123"}, 200)
	e.req("POST", "/api/v1/admin/programs", map[string]string{"slug": "onetaskbar", "name": "One TaskBar"}, 201)

	if code, _ := get(t, e.ts.URL+"/onetaskbar/latest.json"); code != 404 {
		t.Fatalf("no release yet: got %d", code)
	}

	publish := func(ver, file string, data []byte, extra map[string]any) {
		body := map[string]any{"version": ver, "notes": "notes " + ver}
		for k, v := range extra {
			body[k] = v
		}
		e.req("POST", "/api/v1/admin/programs/onetaskbar/releases", body, 201)
		e.uploadTo("/api/v1/admin/programs/onetaskbar/releases/"+ver+"/assets", "file", "windows-amd64", file, data, 201)
		e.req("POST", "/api/v1/admin/programs/onetaskbar/releases/"+ver+"/publish", nil, 200)
	}
	publish("1.0.0.9", "OneTaskBar-Setup.exe", []byte("setup-9"), nil)
	publish("1.0.0.10", "OneTaskBar-Setup-1.0.0.10.exe", []byte("setup-10"), nil)
	publish("1.1.0-beta.1", "OneTaskBar-Setup.exe", []byte("beta"), map[string]any{"channel": "beta"})
	e.req("POST", "/api/v1/admin/programs/onetaskbar/releases", map[string]any{"version": "1.0.0.10.0"}, 400)
	e.req("POST", "/api/v1/admin/programs/onetaskbar/releases", map[string]any{"version": "v1.0.0.10"}, 409)

	base, _ := url.Parse(e.ts.URL + "/onetaskbar/") // what the client stores as UpdateServer
	code, raw := get(t, base.ResolveReference(&url.URL{Path: "latest.json"}).String())
	if code != 200 {
		t.Fatalf("latest.json: %d %s", code, raw)
	}
	var m struct{ Version, File, SHA256, Notes string }
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// 1.0.0.10 > 1.0.0.9 numerically; beta is skipped (not a .NET Version).
	if m.Version != "1.0.0.10" || m.File != "OneTaskBar-Setup-1.0.0.10.exe" || m.Notes != "notes 1.0.0.10" {
		t.Fatalf("manifest = %+v", m)
	}
	code, data := get(t, base.ResolveReference(&url.URL{Path: m.File}).String())
	sum := sha256.Sum256(data)
	if code != 200 || string(data) != "setup-10" || hex.EncodeToString(sum[:]) != m.SHA256 {
		t.Fatalf("download %d %q", code, data)
	}
	// Older version still reachable under its versioned name (name had no version).
	if code, data := get(t, e.ts.URL+"/onetaskbar/OneTaskBar-Setup-1.0.0.9.exe"); code != 200 || string(data) != "setup-9" {
		t.Fatalf("old version %d %q", code, data)
	}
	// Beta clients may opt in with ?channel=beta, but non-numeric versions are never offered.
	if _, raw := get(t, e.ts.URL+"/onetaskbar/latest.json?channel=beta"); !bytes.Contains(raw, []byte(`"1.0.0.10"`)) {
		t.Fatalf("beta latest.json: %s", raw)
	}

	// Fixed files (defaults.json): upload, replace, serve, delete.
	e.uploadTo("/api/v1/admin/programs/onetaskbar/files", "file", "", "defaults.json", []byte(`{"a":1}`), 201)
	e.uploadTo("/api/v1/admin/programs/onetaskbar/files", "file", "", "defaults.json", []byte(`{"a":2}`), 201)
	e.uploadTo("/api/v1/admin/programs/onetaskbar/files", "file", "", "latest.json", []byte(`{}`), 400)
	if code, data := get(t, e.ts.URL+"/onetaskbar/defaults.json"); code != 200 || string(data) != `{"a":2}` {
		t.Fatalf("defaults.json %d %q", code, data)
	}
	e.req("DELETE", "/api/v1/admin/programs/onetaskbar/files/defaults.json", nil, 200)
	if code, _ := get(t, e.ts.URL+"/onetaskbar/defaults.json"); code != 404 {
		t.Fatalf("deleted defaults.json still served: %d", code)
	}

	// Yanking the newest version moves latest.json back.
	e.req("POST", "/api/v1/admin/programs/onetaskbar/releases/1.0.0.10/yank", nil, 200)
	if _, raw := get(t, e.ts.URL+"/onetaskbar/latest.json"); !bytes.Contains(raw, []byte(`"1.0.0.9"`)) {
		t.Fatalf("after yank: %s", raw)
	}
	if code, _ := get(t, e.ts.URL+"/onetaskbar/OneTaskBar-Setup-1.0.0.10.exe"); code != 404 {
		t.Fatalf("yanked exe still public: %d", code)
	}
	// UI and API routes are unaffected.
	if code, _ := get(t, e.ts.URL+"/unknown/latest.json"); code != 404 {
		t.Fatal("unknown program must 404")
	}
	if code, _ := get(t, e.ts.URL+"/api/v1/programs"); code != 200 {
		t.Fatal("api route broken")
	}
}
