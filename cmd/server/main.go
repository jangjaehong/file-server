// Command server runs the file / auto-update server.
//
//	server [flags]                         run the server
//	server set-admin -user NAME -password PW  create admin or reset password
//	server healthcheck                        exit 0 if the local server answers /healthz
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jangjaehong/file-server/internal/server"
	"github.com/jangjaehong/file-server/internal/store"
	"github.com/jangjaehong/file-server/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "set-admin" {
		setAdmin(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck() // used by Docker HEALTHCHECK (the image has no shell/curl)
		return
	}

	addr := flag.String("addr", env("FS_ADDR", ":8080"), "listen address")
	dataDir := flag.String("data", env("FS_DATA", "./data"), "data directory (database + files)")
	baseURL := flag.String("base-url", env("FS_BASE_URL", ""), "public base URL used in update responses (e.g. https://files.example.com)")
	maxMB := flag.Int64("max-upload-mb", 4096, "maximum size of one uploaded file in MB")
	secure := flag.Bool("secure-cookies", os.Getenv("FS_SECURE_COOKIES") == "1", "mark cookies Secure (enable when served over HTTPS)")
	tlsCert := flag.String("tls-cert", env("FS_TLS_CERT", ""), "TLS certificate file (optional)")
	tlsKey := flag.String("tls-key", env("FS_TLS_KEY", ""), "TLS key file (optional)")
	flag.Parse()

	st := openStore(*dataDir)
	defer st.Close()
	bootstrapAdmin(st)

	srv, err := server.New(server.Config{
		DataDir: *dataDir, BaseURL: *baseURL, MaxUploadSize: *maxMB << 20,
		SecureCookies: *secure || *tlsCert != "",
	}, st, web.Static())
	if err != nil {
		log.Fatal(err)
	}

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No Read/WriteTimeout: large uploads/downloads may take a long time.
	}
	go func() {
		log.Printf("file server listening on %s (data: %s)", *addr, *dataDir)
		var err error
		if *tlsCert != "" {
			err = hs.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = hs.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
}

func openStore(dataDir string) *store.Store {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dataDir, "fileserver.db"))
	if err != nil {
		log.Fatal(err)
	}
	return st
}

// bootstrapAdmin creates the first admin from FS_ADMIN_USER / FS_ADMIN_PASSWORD,
// or generates a random password and prints it once.
func bootstrapAdmin(st *store.Store) {
	ctx := context.Background()
	n, err := st.CountAdmins(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if n > 0 {
		return
	}
	user, pw := env("FS_ADMIN_USER", "admin"), os.Getenv("FS_ADMIN_PASSWORD")
	generated := pw == ""
	if generated {
		b := make([]byte, 9)
		_, _ = rand.Read(b)
		pw = hex.EncodeToString(b)
	}
	if err := st.UpsertAdmin(ctx, user, pw); err != nil {
		log.Fatal(err)
	}
	if generated {
		log.Printf("==== initial admin created: user=%q password=%q (change it with `server set-admin`) ====", user, pw)
	} else {
		log.Printf("initial admin %q created from environment", user)
	}
}

func setAdmin(args []string) {
	fs := flag.NewFlagSet("set-admin", flag.ExitOnError)
	dataDir := fs.String("data", env("FS_DATA", "./data"), "data directory")
	user := fs.String("user", "admin", "admin username")
	pw := fs.String("password", "", "new password (min 8 chars)")
	_ = fs.Parse(args)
	st := openStore(*dataDir)
	defer st.Close()
	if err := st.UpsertAdmin(context.Background(), *user, *pw); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("admin %q saved\n", *user)
}

func healthcheck() {
	port := env("FS_ADDR", ":8080")
	if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i+1:]
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
