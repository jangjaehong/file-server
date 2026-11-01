// Command fsctl is the command-line client of the file server.
//
// Uploaders (CI / release script):
//
//	fsctl publish -server URL -token TOKEN -program my-app -version 1.2.3 \
//	      -notes-file CHANGELOG-1.2.3.md -file windows-amd64=dist/my-app.exe -file linux-amd64=dist/my-app
//
// Users / installers:
//
//	fsctl check    -server URL -program my-app -version 1.2.0
//	fsctl update   -server URL -program my-app -version 1.2.0 -target "C:\Program Files\MyApp\my-app.exe"
//	fsctl download -server URL -program my-app [-version latest] [-platform windows-amd64] [-o dir]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/jangjaehong/file-server/pkg/updater"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: fsctl <command> [flags]

commands:
  publish   create a release, upload files and publish it (needs -token)
  check     check whether an update is available
  update    download, verify and install the newest version over -target
  download  download a release file

environment: FS_SERVER, FS_TOKEN can replace -server / -token
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "publish":
		err = cmdPublish(ctx, os.Args[2:])
	case "check":
		err = cmdCheck(ctx, os.Args[2:], false)
	case "update":
		err = cmdCheck(ctx, os.Args[2:], true)
	case "download":
		err = cmdDownload(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func common(fs *flag.FlagSet) (server, program, platform, channel *string) {
	server = fs.String("server", os.Getenv("FS_SERVER"), "server base URL, e.g. http://files.local:8080")
	program = fs.String("program", "", "program slug")
	platform = fs.String("platform", updater.DefaultPlatform(), "platform (e.g. windows-amd64, linux-amd64, any)")
	channel = fs.String("channel", "stable", "stable or beta")
	return
}

// ---------------------------------------------------------------- check / update

func cmdCheck(ctx context.Context, args []string, install bool) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	server, program, platform, channel := common(fs)
	version := fs.String("version", "", "currently installed version")
	target := fs.String("target", "", "update: file to replace (default: only download)")
	jsonOut := fs.Bool("json", false, "check: print raw JSON")
	_ = fs.Parse(args)
	if *server == "" || *program == "" || *version == "" {
		return errors.New("-server, -program and -version are required")
	}
	c := updater.New(*server, *program)
	c.Platform, c.Channel = *platform, *channel
	info, err := c.Check(ctx, *version)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	if !info.UpdateAvailable {
		fmt.Printf("%s %s is up to date\n", *program, *version)
		return nil
	}
	fmt.Printf("update available: %s -> %s", *version, info.Latest.Version)
	if info.Mandatory {
		fmt.Print("  [MANDATORY]")
	}
	fmt.Println()
	for _, ch := range info.Changes {
		fmt.Printf("\n== %s ==\n%s\n", ch.Version, strings.TrimSpace(ch.Notes))
	}
	if !install {
		return nil
	}
	path, err := c.Download(ctx, info, "", printProgress)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Println("downloaded and verified:", path)
	if *target != "" {
		if err := updater.ReplaceFile(*target, path); err != nil {
			return err
		}
		fmt.Println("installed to", *target, "(previous version kept as", *target+".old)")
	}
	return nil
}

func printProgress(done, total int64) {
	if total > 0 {
		fmt.Fprintf(os.Stderr, "\r%6.1f%% %d/%d bytes", float64(done)*100/float64(total), done, total)
	}
}

// ---------------------------------------------------------------- download

func cmdDownload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	server, program, platform, channel := common(fs)
	version := fs.String("version", "latest", "version or latest")
	out := fs.String("o", ".", "output directory")
	_ = fs.Parse(args)
	if *server == "" || *program == "" {
		return errors.New("-server and -program are required")
	}
	u := fmt.Sprintf("%s/download/%s/%s/%s?channel=%s", strings.TrimRight(*server, "/"),
		url.PathEscape(*program), url.PathEscape(*version), url.PathEscape(*platform), url.QueryEscape(*channel))
	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	name := *program
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if i := strings.Index(cd, `filename="`); i >= 0 {
			rest := cd[i+10:]
			if j := strings.IndexByte(rest, '"'); j > 0 {
				name = filepath.Base(rest[:j])
			}
		}
	}
	dst := filepath.Join(*out, name)
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("saved %s (%d bytes, sha256 %s)\n", dst, n, resp.Header.Get("X-Checksum-SHA256"))
	return nil
}

// ---------------------------------------------------------------- publish

type api struct {
	base, token string
}

func (a *api) do(ctx context.Context, method, path string, body io.Reader, ctype string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
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
		return resp.StatusCode, fmt.Errorf("%s %s: %s %s", method, path, resp.Status, e.Error)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

func (a *api) json(ctx context.Context, method, path string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	return a.do(ctx, method, path, bytes.NewReader(b), "application/json", out)
}

func cmdPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	server := fs.String("server", os.Getenv("FS_SERVER"), "server base URL")
	token := fs.String("token", os.Getenv("FS_TOKEN"), "API token (create one in the admin page)")
	program := fs.String("program", "", "program slug")
	name := fs.String("name", "", "program display name (creates the program if it does not exist)")
	version := fs.String("version", "", "semantic version, e.g. 1.2.3 or 1.3.0-beta.1")
	channel := fs.String("channel", "stable", "stable or beta")
	notes := fs.String("notes", "", "patch notes (Markdown)")
	notesFile := fs.String("notes-file", "", "read patch notes from file")
	mandatory := fs.Bool("mandatory", false, "clients must install this update")
	draft := fs.Bool("draft", false, "upload only, do not publish")
	var files multiFlag
	fs.Var(&files, "file", "platform=path, repeatable (e.g. -file windows-amd64=dist/app.exe)")
	_ = fs.Parse(args)

	if *server == "" || *token == "" || *program == "" || *version == "" || len(files) == 0 {
		return errors.New("-server, -token, -program, -version and at least one -file are required")
	}
	if *notesFile != "" {
		b, err := os.ReadFile(*notesFile)
		if err != nil {
			return err
		}
		*notes = string(b)
	}
	a := &api{base: strings.TrimRight(*server, "/"), token: *token}
	slug := url.PathEscape(*program)

	if *name != "" {
		code, err := a.json(ctx, "POST", "/api/v1/admin/programs", map[string]string{"slug": *program, "name": *name}, nil)
		if err != nil && code != http.StatusConflict {
			return err
		}
	}
	code, err := a.json(ctx, "POST", "/api/v1/admin/programs/"+slug+"/releases",
		map[string]any{"version": *version, "channel": *channel, "notes": *notes, "mandatory": *mandatory}, nil)
	if err != nil {
		if code != http.StatusConflict {
			return err
		}
		fmt.Println("release already exists; continuing (works only while it is a draft)")
	} else {
		fmt.Println("created draft", *program, *version)
	}
	rpath := "/api/v1/admin/programs/" + slug + "/releases/" + url.PathEscape(*version)

	for _, spec := range files {
		platform, path, ok := strings.Cut(spec, "=")
		if !ok {
			return fmt.Errorf("-file %q: expected platform=path", spec)
		}
		if err := uploadFile(ctx, a, rpath+"/assets", platform, path); err != nil {
			return err
		}
		fmt.Printf("uploaded %s (%s)\n", path, platform)
	}
	if *draft {
		fmt.Println("left as draft; publish it from the admin page")
		return nil
	}
	if _, err := a.do(ctx, "POST", rpath+"/publish", nil, "", nil); err != nil {
		return err
	}
	fmt.Printf("published %s %s\n", *program, *version)
	return nil
}

// uploadFile streams a file as multipart without loading it into memory.
func uploadFile(ctx context.Context, a *api, path, platform, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := mw.WriteField("platform", platform)
		if err == nil {
			var part io.Writer
			part, err = mw.CreateFormFile("file", filepath.Base(file))
			if err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	_, err = a.do(ctx, "POST", path, pr, mw.FormDataContentType(), nil)
	return err
}
