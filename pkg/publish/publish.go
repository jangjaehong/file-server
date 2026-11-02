// Package publish uploads a build to the file server as a new version.
// It is what `fsctl publish` runs, and tools/Publish-FileServer.ps1 implements
// the same steps in PowerShell.
//
// The operation is safe to repeat (CI re-runs):
//
//  1. the program is created when it does not exist yet (first deployment);
//  2. if the version does not exist it is created, the file uploaded and published;
//  3. if the version is a draft, the upload/publish is resumed
//     (a file of the same platform with a different checksum is replaced);
//  4. if the version is already published with the same SHA-256 nothing happens
//     (Result.AlreadyPublished); with a different SHA-256 it fails, because
//     published versions are immutable — build a new version number instead.
package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options describes one publish.
type Options struct {
	Server    string // base URL, e.g. http://211.231.121.41:9100
	Token     string // API token (Bearer)
	Program   string // slug, e.g. onetaskbar
	Name      string // display name used when the program is created (default: slug)
	Version   string // e.g. 1.0.0.57
	Channel   string // stable (default) or beta
	Notes     string
	Mandatory bool
	Draft     bool              // upload only, do not publish
	Files     map[string]string // platform -> local path
	Fixed     []string          // unversioned files (defaults.json), replaced on every upload
	HTTP      *http.Client
	Log       func(format string, args ...any)
}

// Result reports what happened.
type Result struct {
	ProgramCreated   bool
	AlreadyPublished bool // same version + same files were already published: nothing done
	Uploaded         []string
	Published        bool
	LatestJSON       string // <server>/<program>/latest.json
}

// ErrImmutable is returned when a published version would get a different file.
var ErrImmutable = errors.New("version already published with a different file")

type release struct {
	Version   string `json:"version"`
	Status    string `json:"status"`
	Channel   string `json:"channel"`
	Notes     string `json:"notes"`
	Mandatory bool   `json:"mandatory"`
	Assets    []struct {
		ID       int64  `json:"id"`
		Platform string `json:"platform"`
		Filename string `json:"filename"`
		SHA256   string `json:"sha256"`
	} `json:"assets"`
}

type client struct {
	base, token string
	hc          *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body io.Reader, ctype string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return resp.StatusCode, fmt.Errorf("%s %s: %s %s", method, path, resp.Status, e.Error)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

func (c *client) json(ctx context.Context, method, path string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	return c.do(ctx, method, path, bytes.NewReader(b), "application/json", out)
}

// Run performs the publish described by o.
func Run(ctx context.Context, o Options) (*Result, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Server == "" || o.Token == "" || o.Program == "" {
		return nil, errors.New("server, token and program are required")
	}
	if len(o.Files) > 0 && o.Version == "" {
		return nil, errors.New("version is required")
	}
	if len(o.Files) == 0 && len(o.Fixed) == 0 {
		return nil, errors.New("nothing to publish: give a file and/or a fixed file")
	}
	if o.Channel == "" {
		o.Channel = "stable"
	}
	if o.Name == "" {
		o.Name = o.Program
	}
	hc := o.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	c := &client{base: strings.TrimRight(o.Server, "/"), token: o.Token, hc: hc}
	res := &Result{LatestJSON: c.base + "/" + o.Program + "/latest.json"}
	slug := url.PathEscape(o.Program)

	// 1. program (created on first deployment)
	code, err := c.json(ctx, "POST", "/api/v1/admin/programs", map[string]string{"slug": o.Program, "name": o.Name}, nil)
	switch {
	case err == nil:
		res.ProgramCreated = true
		logf("created program %s (%s)", o.Program, o.Name)
	case code != http.StatusConflict:
		return nil, err
	}

	if len(o.Files) > 0 {
		if err := publishVersion(ctx, c, slug, o, res, logf); err != nil {
			return res, err
		}
	}

	for _, f := range o.Fixed {
		if err := upload(ctx, c, "/api/v1/admin/programs/"+slug+"/files", "", f); err != nil {
			return res, err
		}
		logf("uploaded fixed file %s -> %s/%s/%s", filepath.Base(f), c.base, o.Program, filepath.Base(f))
	}
	return res, nil
}

func publishVersion(ctx context.Context, c *client, slug string, o Options, res *Result, logf func(string, ...any)) error {
	hashes := map[string]string{}
	for platform, path := range o.Files {
		h, err := FileSHA256(path)
		if err != nil {
			return err
		}
		hashes[platform] = h
	}
	rpath := "/api/v1/admin/programs/" + slug + "/releases/" + url.PathEscape(o.Version)

	// 2. existing version?
	var rel release
	code, err := c.do(ctx, "GET", rpath, nil, "", &rel)
	switch {
	case code == http.StatusNotFound:
		if _, err := c.json(ctx, "POST", "/api/v1/admin/programs/"+slug+"/releases",
			map[string]any{"version": o.Version, "channel": o.Channel, "notes": o.Notes, "mandatory": o.Mandatory}, &rel); err != nil {
			return err
		}
		logf("created version %s %s", o.Program, o.Version)
	case err != nil:
		return err
	}

	// 3. compare with what is already there
	todo := map[string]string{} // platform -> path still to upload
	for platform, path := range o.Files {
		todo[platform] = path
		for _, a := range rel.Assets {
			if a.Platform != platform {
				continue
			}
			if strings.EqualFold(a.SHA256, hashes[platform]) {
				delete(todo, platform) // identical file already there
			} else if rel.Status == "draft" {
				if _, err := c.do(ctx, "DELETE", fmt.Sprintf("/api/v1/admin/assets/%d", a.ID), nil, "", nil); err != nil {
					return err
				}
				logf("replaced %s/%s in draft %s", platform, a.Filename, o.Version)
			}
		}
	}

	switch rel.Status {
	case "published":
		if len(todo) == 0 {
			res.AlreadyPublished = true
			logf("%s %s is already published with the same file(s); nothing to do", o.Program, o.Version)
			return nil
		}
		return fmt.Errorf("%w: %s %s — rebuild with a new version number", ErrImmutable, o.Program, o.Version)
	case "yanked":
		return fmt.Errorf("%s %s was withdrawn (yanked); publish a new version number", o.Program, o.Version)
	}

	// draft: apply this run's metadata (keep notes typed in the admin page when none given),
	// then upload what is missing
	notes := o.Notes
	if notes == "" {
		notes = rel.Notes
	}
	if _, err := c.json(ctx, "PUT", rpath, map[string]any{"channel": o.Channel, "notes": notes, "mandatory": o.Mandatory || rel.Mandatory}, nil); err != nil {
		return err
	}
	for platform, path := range todo {
		if err := upload(ctx, c, rpath+"/assets", platform, path); err != nil {
			return err
		}
		res.Uploaded = append(res.Uploaded, platform+"="+filepath.Base(path))
		logf("uploaded %s (%s)", filepath.Base(path), platform)
	}
	if o.Draft {
		logf("left %s %s as draft; publish it in the admin page", o.Program, o.Version)
		return nil
	}
	if _, err := c.do(ctx, "POST", rpath+"/publish", nil, "", nil); err != nil {
		return err
	}
	res.Published = true
	logf("published %s %s -> %s", o.Program, o.Version, res.LatestJSON)
	return nil
}

// upload streams one file as multipart without loading it into memory.
func upload(ctx context.Context, c *client, path, platform, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		if platform != "" {
			err = mw.WriteField("platform", platform)
		}
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile("file", filepath.Base(file)); err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	_, err = c.do(ctx, "POST", path, pr, mw.FormDataContentType(), nil)
	return err
}

// FileSHA256 returns the lowercase hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NotesFromGit builds Markdown patch notes from commit subjects.
// rng is a git revision range ("v1.2.0..HEAD") or a single ref meaning "<ref>..HEAD".
func NotesFromGit(dir, rng string) (string, error) {
	if !strings.Contains(rng, "..") {
		rng += "..HEAD"
	}
	cmd := exec.Command("git", "log", "--no-merges", "--pretty=format:- %s", rng)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log %s: %w", rng, err)
	}
	notes := strings.TrimSpace(string(out))
	if notes == "" {
		notes = "- 변경 사항 없음"
	}
	return notes, nil
}

// VersionFromFile reads version.txt next to the given file, if present.
func VersionFromFile(path string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "version.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
}
