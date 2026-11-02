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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/jangjaehong/file-server/pkg/publish"
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

func cmdPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	server := fs.String("server", os.Getenv("FS_SERVER"), "server base URL (env FS_SERVER)")
	token := fs.String("token", os.Getenv("FS_TOKEN"), "API token (env FS_TOKEN)")
	program := fs.String("program", "", "program slug, e.g. onetaskbar")
	name := fs.String("name", "", "display name when the program is created (default: slug)")
	version := fs.String("version", "", "version, e.g. 1.0.0.57 (default: version.txt next to the first -file)")
	channel := fs.String("channel", "stable", "stable or beta")
	notes := fs.String("notes", "", "patch notes (Markdown)")
	notesFile := fs.String("notes-file", "", "read patch notes from file")
	notesGit := fs.String("notes-from-git", "", "generate notes from commit subjects: <ref> (= <ref>..HEAD) or <a>..<b>")
	mandatory := fs.Bool("mandatory", false, "clients must install this update")
	draft := fs.Bool("draft", false, "upload only, do not publish")
	allowOlder := fs.Bool("allow-older", false, "allow a version lower than the newest published one")
	skip := fs.Bool("skip-if-unconfigured", false, "exit 0 without doing anything when server or token is empty (CI without secrets)")
	var files, fixed multiFlag
	fs.Var(&files, "file", "platform=path, repeatable (e.g. -file windows-amd64=dist/app.exe)")
	fs.Var(&fixed, "fixed", "unversioned file such as defaults.json, repeatable (replaced on every upload)")
	_ = fs.Parse(args)

	if *skip && (*server == "" || *token == "") {
		fmt.Println("FS_SERVER/FS_TOKEN not set; skipping publish")
		return nil
	}
	o := publish.Options{
		Server: *server, Token: *token, Program: *program, Name: *name, Version: *version,
		Channel: *channel, Notes: *notes, Mandatory: *mandatory, Draft: *draft, AllowOlder: *allowOlder,
		Files: map[string]string{}, Fixed: fixed,
		Log: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	}
	for _, spec := range files {
		platform, path, ok := strings.Cut(spec, "=")
		if !ok {
			return fmt.Errorf("-file %q: expected platform=path", spec)
		}
		o.Files[platform] = path
		if o.Version == "" {
			o.Version = publish.VersionFromFile(path)
		}
	}
	switch {
	case *notesFile != "":
		b, err := os.ReadFile(*notesFile)
		if err != nil {
			return err
		}
		o.Notes = string(b)
	case *notesGit != "" && o.Notes == "":
		n, err := publish.NotesFromGit(".", *notesGit)
		if err != nil {
			return err
		}
		o.Notes = n
	}
	res, err := publish.Run(ctx, o)
	if err != nil {
		return err
	}
	if len(o.Files) > 0 {
		fmt.Println("latest.json:", res.LatestJSON)
	}
	return nil
}
