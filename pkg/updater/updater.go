// Package updater is a client library that programs embed to update themselves
// from the file server.
//
//	u := updater.New("https://files.example.com", "my-app")
//	info, err := u.Check(ctx, Version)          // Version = "1.2.0" of the running binary
//	if err == nil && info.UpdateAvailable {
//	    path, _ := u.Download(ctx, info, "", nil) // verified by SHA-256
//	    _ = updater.ReplaceExecutable(path)       // swap the running binary
//	    // restart the program ...
//	}
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Client talks to the update API.
type Client struct {
	BaseURL  string // server base URL
	Program  string // program slug
	Platform string // e.g. windows-amd64 (default: runtime GOOS-GOARCH)
	Channel  string // "stable" (default) or "beta"
	HTTP     *http.Client
}

// New creates a client for the current platform on the stable channel.
func New(baseURL, program string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"), Program: program,
		Platform: DefaultPlatform(), Channel: "stable",
		HTTP: &http.Client{Timeout: 0},
	}
}

// DefaultPlatform returns GOOS-GOARCH, e.g. "windows-amd64".
func DefaultPlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Info is the server's answer to an update check.
type Info struct {
	Program         string  `json:"program"`
	CurrentVersion  string  `json:"current_version"`
	UpdateAvailable bool    `json:"update_available"`
	Mandatory       bool    `json:"mandatory"`
	CurrentYanked   bool    `json:"current_yanked"`
	Latest          *Target `json:"latest"`
	Changes         []struct {
		Version     string     `json:"version"`
		Mandatory   bool       `json:"mandatory"`
		PublishedAt *time.Time `json:"published_at"`
		Notes       string     `json:"notes"`
	} `json:"changes"`
}

// Target is the file to install.
type Target struct {
	Version  string `json:"version"`
	Channel  string `json:"channel"`
	Platform string `json:"platform"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	URL      string `json:"url"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Check asks the server whether a newer version than current exists.
func (c *Client) Check(ctx context.Context, current string) (*Info, error) {
	q := url.Values{
		"program": {c.Program}, "version": {current},
		"platform": {c.Platform}, "channel": {c.Channel},
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/update?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("update check failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Progress is called during download with bytes done and total.
type Progress func(done, total int64)

// Download fetches the update into dir (default: os.TempDir()) and verifies its
// SHA-256 checksum. Interrupted downloads are resumed from a ".part" file.
// It returns the path of the verified file.
func (c *Client) Download(ctx context.Context, info *Info, dir string, progress Progress) (string, error) {
	if info == nil || info.Latest == nil {
		return "", errors.New("no update target")
	}
	t := info.Latest
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "fs-update-"+c.Program)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, t.Version+"-"+filepath.Base(t.Filename))
	if ok, _ := fileHasHash(final, t.SHA256); ok {
		return final, nil // already downloaded
	}
	part := final + ".part"
	if err := DownloadURL(ctx, c.httpClient(), t.URL, part, t.Size, progress); err != nil {
		return "", err
	}
	ok, err := fileHasHash(part, t.SHA256)
	if err != nil {
		return "", err
	}
	if !ok {
		_ = os.Remove(part)
		return "", errors.New("checksum mismatch: downloaded file is corrupt, retry")
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(final, 0o755)
	}
	return final, nil
}

// DownloadURL downloads url to path, resuming an existing partial file with a Range request.
func DownloadURL(ctx context.Context, hc *http.Client, rawURL, path string, size int64, progress Progress) error {
	var offset int64
	if fi, err := os.Stat(path); err == nil && (size <= 0 || fi.Size() < size) {
		offset = fi.Size()
	} else if err == nil {
		_ = os.Remove(path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	total := size
	if total <= 0 && resp.ContentLength > 0 {
		total = offset + resp.ContentLength
	}
	var w io.Writer = f
	if progress != nil {
		w = &progressWriter{w: f, done: offset, total: total, fn: progress}
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return err
	}
	return f.Sync()
}

type progressWriter struct {
	w           io.Writer
	done, total int64
	fn          Progress
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	p.fn(p.done, p.total)
	return n, err
}

func fileHasHash(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want), nil
}

// ReplaceExecutable swaps the running executable with newFile.
// The old binary is kept as "<exe>.old" (Windows cannot delete a running exe,
// but it can rename it). Call CleanupOld at the next start.
func ReplaceExecutable(newFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	return ReplaceFile(exe, newFile)
}

// ReplaceFile atomically replaces target with newFile, keeping target+".old" as backup.
// On failure the original file is restored.
func ReplaceFile(target, newFile string) error {
	dir := filepath.Dir(target)
	staged := filepath.Join(dir, "."+filepath.Base(target)+".new")
	if err := copyFile(newFile, staged); err != nil {
		return fmt.Errorf("stage update: %w", err)
	}
	old := target + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			_ = os.Remove(staged)
			return fmt.Errorf("move current file aside: %w", err)
		}
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(old, target) // roll back
		_ = os.Remove(staged)
		return fmt.Errorf("install update: %w", err)
	}
	return nil
}

// CleanupOld removes the "<exe>.old" left behind by ReplaceExecutable.
func CleanupOld() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := os.FileMode(0o755)
	if fi, err := in.Stat(); err == nil && runtime.GOOS != "windows" {
		mode = fi.Mode().Perm() | 0o100
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
