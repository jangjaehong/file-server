// Package server implements the HTTP API, file storage and web UI.
package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jangjaehong/file-server/internal/store"
)

// Config configures the server.
type Config struct {
	DataDir       string        // root for uploaded files
	BaseURL       string        // public URL (e.g. https://files.example.com); derived from request if empty
	MaxUploadSize int64         // bytes per uploaded file
	SessionTTL    time.Duration // admin login lifetime
	SecureCookies bool          // set Secure flag on cookies (use behind HTTPS)
}

// Server holds dependencies for the HTTP handlers.
type Server struct {
	cfg     Config
	st      *store.Store
	static  fs.FS
	limiter *loginLimiter
}

// New creates a server. static is the web UI file system.
func New(cfg Config, st *store.Store, static fs.FS) (*Server, error) {
	if cfg.MaxUploadSize <= 0 {
		cfg.MaxUploadSize = 4 << 30
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "files"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "tmp"), 0o755); err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, st: st, static: static, limiter: newLoginLimiter()}, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- public API (no login)
	mux.HandleFunc("GET /api/v1/programs", s.handleListPrograms)
	mux.HandleFunc("GET /api/v1/programs/{slug}", s.handleGetProgram)
	mux.HandleFunc("GET /api/v1/programs/{slug}/releases", s.handleListReleases)
	mux.HandleFunc("GET /api/v1/programs/{slug}/releases/{version}", s.handleGetRelease)
	mux.HandleFunc("GET /api/v1/update", s.handleUpdateCheck) // auto-update endpoint
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})

	// ---- downloads (no login). "latest" is accepted as version.
	mux.HandleFunc("GET /download/{slug}/{version}/{platform}", s.handleDownloadByPlatform)
	mux.HandleFunc("GET /download/{slug}/{version}/{platform}/{filename}", s.handleDownloadByName)
	mux.HandleFunc("GET /files/{id}", s.handleDownloadByID)

	// ---- auth
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/v1/auth/me", s.handleMe)

	// ---- admin API (session cookie or API token)
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAdmin(h)) }
	admin("GET /api/v1/admin/programs", s.handleAdminListPrograms)
	admin("POST /api/v1/admin/programs", s.handleCreateProgram)
	admin("PUT /api/v1/admin/programs/{slug}", s.handleUpdateProgram)
	admin("DELETE /api/v1/admin/programs/{slug}", s.handleDeleteProgram)
	admin("GET /api/v1/admin/programs/{slug}/releases", s.handleAdminListReleases)
	admin("POST /api/v1/admin/programs/{slug}/releases", s.handleCreateRelease)
	admin("PUT /api/v1/admin/programs/{slug}/releases/{version}", s.handleUpdateRelease)
	admin("DELETE /api/v1/admin/programs/{slug}/releases/{version}", s.handleDeleteRelease)
	admin("POST /api/v1/admin/programs/{slug}/releases/{version}/publish", s.handleSetStatus(store.StatusPublished))
	admin("POST /api/v1/admin/programs/{slug}/releases/{version}/yank", s.handleSetStatus(store.StatusYanked))
	admin("POST /api/v1/admin/programs/{slug}/releases/{version}/unpublish", s.handleSetStatus(store.StatusDraft))
	admin("POST /api/v1/admin/programs/{slug}/releases/{version}/assets", s.handleUploadAsset)
	admin("DELETE /api/v1/admin/assets/{id}", s.handleDeleteAsset)
	admin("GET /api/v1/admin/tokens", s.handleListTokens)
	admin("POST /api/v1/admin/tokens", s.handleCreateToken)
	admin("DELETE /api/v1/admin/tokens/{id}", s.handleDeleteToken)

	// ---- web UI (single page app with hash routing)
	mux.Handle("GET /", http.FileServerFS(s.static))

	return logRequests(securityHeaders(mux))
}

// ---------------------------------------------------------------- helpers

var (
	slugRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	platformRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// storeErr maps store errors to HTTP status codes.
func storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "already exists")
	default:
		log.Printf("internal error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// baseURL returns the externally visible base URL.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}
