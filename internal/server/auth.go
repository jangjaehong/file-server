package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "fs_session"

type ctxKey struct{}

// randomToken returns a 32-byte random hex token.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// actor returns who performed the admin request ("admin:alice" or "token:ci").
func actor(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// authenticate checks a bearer API token or the session cookie.
func (s *Server) authenticate(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		name, err := s.st.UseAPIToken(r.Context(), hashToken(strings.TrimPrefix(h, "Bearer ")))
		if err != nil {
			return "", false
		}
		return "token:" + name, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	name, err := s.st.SessionAdmin(r.Context(), hashToken(c.Value))
	if err != nil {
		return "", false
	}
	return "admin:" + name, true
}

// requireAdmin protects admin endpoints. Cookie-authenticated, state-changing
// requests must carry the X-Requested-With header (simple CSRF defence: a
// cross-site form cannot set custom headers, and SameSite=Strict blocks the cookie).
func (s *Server) requireAdmin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, ok := s.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		if strings.HasPrefix(who, "admin:") && r.Method != http.MethodGet && r.Header.Get("X-Requested-With") == "" {
			writeErr(w, http.StatusForbidden, "missing X-Requested-With header")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, who)))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts, try again later")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	a, err := s.st.GetAdmin(r.Context(), req.Username)
	if err != nil || !a.CheckPassword(req.Password) {
		s.limiter.fail(ip)
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.limiter.reset(ip)
	tok := randomToken()
	exp := time.Now().Add(s.cfg.SessionTTL)
	if err := s.st.CreateSession(r.Context(), hashToken(tok), a.ID, exp); err != nil {
		storeErr(w, err)
		return
	}
	_ = s.st.PurgeExpiredSessions(r.Context())
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", Expires: exp,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.cfg.SecureCookies,
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": a.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.st.DeleteSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	who, ok := s.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not logged in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": who})
}

// ---- API tokens (for CI / fsctl publish)

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.st.ListAPITokens(r.Context())
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	secret := "fst_" + randomToken()
	t, err := s.st.CreateAPIToken(r.Context(), strings.TrimSpace(req.Name), hashToken(secret))
	if err != nil {
		storeErr(w, err)
		return
	}
	// The secret is shown exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{"token": t, "secret": secret})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteAPIToken(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- brute-force protection

type loginLimiter struct {
	mu   sync.Mutex
	hits map[string]*limitEntry
}

type limitEntry struct {
	fails int
	until time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{hits: map[string]*limitEntry{}} }

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.hits[ip]
	return e == nil || time.Now().After(e.until)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.hits[ip]
	if e == nil {
		e = &limitEntry{}
		l.hits[ip] = e
	}
	e.fails++
	if e.fails >= 5 {
		e.until = time.Now().Add(time.Minute * time.Duration(e.fails-4))
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.hits, ip)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
