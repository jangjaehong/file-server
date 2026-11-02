// Package store persists programs, releases, assets and admin accounts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO needed)

	"github.com/jangjaehong/file-server/internal/semver"
)

// Errors returned by the store.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// Release statuses.
const (
	StatusDraft     = "draft"     // being prepared; invisible to the public
	StatusPublished = "published" // visible & offered by the update API
	StatusYanked    = "yanked"    // withdrawn; kept for history, never offered
)

// Release channels.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Program is an application distributed by the server.
type Program struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"` // Markdown
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Filled by ListPrograms for convenience.
	LatestVersion string `json:"latest_version,omitempty"`
	LatestBeta    string `json:"latest_beta,omitempty"`
	ReleaseCount  int    `json:"release_count"`
}

// Release is one immutable version of a program.
type Release struct {
	ID          int64      `json:"id"`
	ProgramID   int64      `json:"program_id"`
	Version     string     `json:"version"`
	Channel     string     `json:"channel"`
	Notes       string     `json:"notes"` // patch notes, Markdown
	Mandatory   bool       `json:"mandatory"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Assets      []Asset    `json:"assets"`
}

// Asset is a downloadable file attached to a release (one per platform/file).
type Asset struct {
	ID          int64     `json:"id"`
	ReleaseID   int64     `json:"release_id"`
	Platform    string    `json:"platform"` // e.g. windows-amd64, linux-arm64, any
	Filename    string    `json:"filename"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	StoragePath string    `json:"-"`
	Downloads   int64     `json:"downloads"`
	CreatedAt   time.Time `json:"created_at"`
}

// APIToken allows non-interactive (CI / CLI) uploads.
type APIToken struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// Admin is an administrator account.
type Admin struct {
	ID       int64
	Username string
	hash     string
}

// CheckPassword verifies pw against the stored bcrypt hash.
func (a *Admin) CheckPassword(pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(a.hash), []byte(pw)) == nil
}

const schema = `
CREATE TABLE IF NOT EXISTS admins (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	admin_id   INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_tokens (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_used  INTEGER
);
CREATE TABLE IF NOT EXISTS programs (
	id          INTEGER PRIMARY KEY,
	slug        TEXT NOT NULL UNIQUE,
	name        TEXT NOT NULL,
	summary     TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS releases (
	id           INTEGER PRIMARY KEY,
	program_id   INTEGER NOT NULL REFERENCES programs(id) ON DELETE CASCADE,
	version      TEXT NOT NULL,
	channel      TEXT NOT NULL DEFAULT 'stable',
	notes        TEXT NOT NULL DEFAULT '',
	mandatory    INTEGER NOT NULL DEFAULT 0,
	status       TEXT NOT NULL DEFAULT 'draft',
	created_at   INTEGER NOT NULL,
	published_at INTEGER,
	UNIQUE(program_id, version)
);
CREATE TABLE IF NOT EXISTS assets (
	id           INTEGER PRIMARY KEY,
	release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
	platform     TEXT NOT NULL,
	filename     TEXT NOT NULL,
	size         INTEGER NOT NULL,
	sha256       TEXT NOT NULL,
	storage_path TEXT NOT NULL,
	downloads    INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	UNIQUE(release_id, platform, filename)
);
CREATE INDEX IF NOT EXISTS idx_releases_program ON releases(program_id);
CREATE INDEX IF NOT EXISTS idx_assets_release ON assets(release_id);
`

// Open opens (and creates/migrates) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers, avoids SQLITE_BUSY
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().Unix() }

func unix(t int64) time.Time { return time.Unix(t, 0).UTC() }

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------------------------------------------------------------- admins

// CountAdmins returns the number of admin accounts.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// MinPasswordLen is the minimum admin password length.
const MinPasswordLen = 4

// UpsertAdmin creates the admin or resets its password.
func (s *Store) UpsertAdmin(ctx context.Context, username, password string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("username required")
	}
	if len(password) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO admins(username, password_hash, created_at) VALUES(?,?,?)
		ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash`, username, string(h), now())
	return err
}

// GetAdmin returns the admin with the given username.
func (s *Store) GetAdmin(ctx context.Context, username string) (*Admin, error) {
	a := &Admin{}
	err := s.db.QueryRowContext(ctx, `SELECT id, username, password_hash FROM admins WHERE username=?`, username).
		Scan(&a.ID, &a.Username, &a.hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ---------------------------------------------------------------- sessions

// CreateSession stores a login session.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, adminID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash, admin_id, expires_at) VALUES(?,?,?)`,
		tokenHash, adminID, expires.Unix())
	return err
}

// SessionAdmin returns the username for a valid session.
func (s *Store) SessionAdmin(ctx context.Context, tokenHash string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT a.username FROM sessions s JOIN admins a ON a.id=s.admin_id
		WHERE s.token_hash=? AND s.expires_at>?`, tokenHash, now()).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// PurgeExpiredSessions deletes expired sessions.
func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, now())
	return err
}

// ---------------------------------------------------------------- api tokens

// CreateAPIToken stores a new token hash.
func (s *Store) CreateAPIToken(ctx context.Context, name, tokenHash string) (*APIToken, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(name, token_hash, created_at) VALUES(?,?,?)`, name, tokenHash, ts)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &APIToken{ID: id, Name: name, CreatedAt: unix(ts)}, nil
}

// ListAPITokens lists all tokens (without secrets).
func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, last_used FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var c int64
		var lu sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &c, &lu); err != nil {
			return nil, err
		}
		t.CreatedAt = unix(c)
		if lu.Valid {
			x := unix(lu.Int64)
			t.LastUsed = &x
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes a token.
func (s *Store) DeleteAPIToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id=?`, id)
	return affected(res, err)
}

// UseAPIToken validates a token hash and records its use. Returns the token name.
func (s *Store) UseAPIToken(ctx context.Context, tokenHash string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `UPDATE api_tokens SET last_used=? WHERE token_hash=? RETURNING name`, now(), tokenHash).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// ---------------------------------------------------------------- programs

const programCols = `id, slug, name, summary, description, created_at, updated_at`

func scanProgram(sc interface{ Scan(...any) error }) (*Program, error) {
	p := &Program{}
	var c, u int64
	if err := sc.Scan(&p.ID, &p.Slug, &p.Name, &p.Summary, &p.Description, &c, &u); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = unix(c), unix(u)
	return p, nil
}

// ListPrograms returns all programs with their latest published versions.
// When includeEmpty is false, programs without a published release are hidden.
func (s *Store) ListPrograms(ctx context.Context, includeEmpty bool) ([]Program, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+programCols+` FROM programs ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	var progs []*Program
	for rows.Next() {
		p, err := scanProgram(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		progs = append(progs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []Program{}
	for _, p := range progs {
		rels, err := s.ListReleases(ctx, p.ID, false)
		if err != nil {
			return nil, err
		}
		p.ReleaseCount = len(rels)
		if r := Latest(rels, ChannelStable); r != nil {
			p.LatestVersion = r.Version
		}
		if r := Latest(rels, ChannelBeta); r != nil && r.Version != p.LatestVersion {
			p.LatestBeta = r.Version
		}
		if includeEmpty || p.ReleaseCount > 0 {
			out = append(out, *p)
		}
	}
	return out, nil
}

// GetProgram returns a program by slug.
func (s *Store) GetProgram(ctx context.Context, slug string) (*Program, error) {
	p, err := scanProgram(s.db.QueryRowContext(ctx, `SELECT `+programCols+` FROM programs WHERE slug=?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// CreateProgram inserts a new program.
func (s *Store) CreateProgram(ctx context.Context, p *Program) error {
	ts := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO programs(slug, name, summary, description, created_at, updated_at)
		VALUES(?,?,?,?,?,?)`, p.Slug, p.Name, p.Summary, p.Description, ts, ts)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	p.CreatedAt, p.UpdatedAt = unix(ts), unix(ts)
	return nil
}

// UpdateProgram updates name/summary/description.
func (s *Store) UpdateProgram(ctx context.Context, p *Program) error {
	res, err := s.db.ExecContext(ctx, `UPDATE programs SET name=?, summary=?, description=?, updated_at=? WHERE id=?`,
		p.Name, p.Summary, p.Description, now(), p.ID)
	return affected(res, err)
}

// DeleteProgram deletes a program and (via cascade) its releases and assets rows.
func (s *Store) DeleteProgram(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM programs WHERE id=?`, id)
	return affected(res, err)
}

// ---------------------------------------------------------------- releases

const releaseCols = `id, program_id, version, channel, notes, mandatory, status, created_at, published_at`

func scanRelease(sc interface{ Scan(...any) error }) (*Release, error) {
	r := &Release{}
	var c int64
	var pub sql.NullInt64
	var mand int
	if err := sc.Scan(&r.ID, &r.ProgramID, &r.Version, &r.Channel, &r.Notes, &mand, &r.Status, &c, &pub); err != nil {
		return nil, err
	}
	r.Mandatory = mand != 0
	r.CreatedAt = unix(c)
	if pub.Valid {
		t := unix(pub.Int64)
		r.PublishedAt = &t
	}
	r.Assets = []Asset{}
	return r, nil
}

// ListReleases returns the releases of a program, newest version first, with assets.
// If all is false only published releases are returned.
func (s *Store) ListReleases(ctx context.Context, programID int64, all bool) ([]Release, error) {
	q := `SELECT ` + releaseCols + ` FROM releases WHERE program_id=?`
	if !all {
		q += ` AND status='published'`
	}
	rows, err := s.db.QueryContext(ctx, q, programID)
	if err != nil {
		return nil, err
	}
	var rels []Release
	idx := map[int64]int{}
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		idx[r.ID] = len(rels)
		rels = append(rels, *r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rels) > 0 {
		arows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
			WHERE release_id IN (SELECT id FROM releases WHERE program_id=?) ORDER BY platform, filename`, programID)
		if err != nil {
			return nil, err
		}
		for arows.Next() {
			a, err := scanAsset(arows)
			if err != nil {
				arows.Close()
				return nil, err
			}
			if i, ok := idx[a.ReleaseID]; ok {
				rels[i].Assets = append(rels[i].Assets, *a)
			}
		}
		arows.Close()
		if err := arows.Err(); err != nil {
			return nil, err
		}
	}
	SortReleases(rels)
	if rels == nil {
		rels = []Release{}
	}
	return rels, nil
}

// SortReleases sorts by semantic version, newest first.
func SortReleases(rels []Release) {
	sort.SliceStable(rels, func(i, j int) bool {
		a, errA := semver.Parse(rels[i].Version)
		b, errB := semver.Parse(rels[j].Version)
		if errA != nil || errB != nil {
			return rels[i].Version > rels[j].Version
		}
		return semver.Less(b, a)
	})
}

// Latest returns the newest published release reachable on the given channel.
// The beta channel also receives stable releases (beta testers are never behind stable).
func Latest(rels []Release, channel string) *Release {
	sorted := append([]Release(nil), rels...)
	SortReleases(sorted)
	for i := range sorted {
		r := &sorted[i]
		if r.Status != StatusPublished {
			continue
		}
		if r.Channel == ChannelStable || channel == ChannelBeta {
			return r
		}
	}
	return nil
}

// GetRelease returns a release (with assets) by program and version.
func (s *Store) GetRelease(ctx context.Context, programID int64, version string) (*Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE program_id=? AND version=?`,
		programID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets WHERE release_id=? ORDER BY platform, filename`, r.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		r.Assets = append(r.Assets, *a)
	}
	return r, rows.Err()
}

// CreateRelease inserts a new draft release.
func (s *Store) CreateRelease(ctx context.Context, r *Release) error {
	ts := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO releases(program_id, version, channel, notes, mandatory, status, created_at)
		VALUES(?,?,?,?,?,?,?)`, r.ProgramID, r.Version, r.Channel, r.Notes, b2i(r.Mandatory), StatusDraft, ts)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	r.Status = StatusDraft
	r.CreatedAt = unix(ts)
	r.Assets = []Asset{}
	return nil
}

// UpdateRelease updates the editable metadata of a release.
func (s *Store) UpdateRelease(ctx context.Context, r *Release) error {
	res, err := s.db.ExecContext(ctx, `UPDATE releases SET channel=?, notes=?, mandatory=? WHERE id=?`,
		r.Channel, r.Notes, b2i(r.Mandatory), r.ID)
	return affected(res, err)
}

// SetReleaseStatus changes the lifecycle status.
func (s *Store) SetReleaseStatus(ctx context.Context, id int64, status string) error {
	var res sql.Result
	var err error
	if status == StatusPublished {
		res, err = s.db.ExecContext(ctx, `UPDATE releases SET status=?, published_at=COALESCE(published_at, ?) WHERE id=?`, status, now(), id)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE releases SET status=? WHERE id=?`, status, id)
	}
	return affected(res, err)
}

// DeleteRelease deletes a release row (and its asset rows).
func (s *Store) DeleteRelease(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE id=?`, id)
	return affected(res, err)
}

// ---------------------------------------------------------------- assets

const assetCols = `id, release_id, platform, filename, size, sha256, storage_path, downloads, created_at`

func scanAsset(sc interface{ Scan(...any) error }) (*Asset, error) {
	a := &Asset{}
	var c int64
	if err := sc.Scan(&a.ID, &a.ReleaseID, &a.Platform, &a.Filename, &a.Size, &a.SHA256, &a.StoragePath, &a.Downloads, &c); err != nil {
		return nil, err
	}
	a.CreatedAt = unix(c)
	return a, nil
}

// AddAsset inserts an asset row.
func (s *Store) AddAsset(ctx context.Context, a *Asset) error {
	ts := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO assets(release_id, platform, filename, size, sha256, storage_path, created_at)
		VALUES(?,?,?,?,?,?,?)`, a.ReleaseID, a.Platform, a.Filename, a.Size, a.SHA256, a.StoragePath, ts)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	a.CreatedAt = unix(ts)
	return nil
}

// GetAsset returns an asset by id.
func (s *Store) GetAsset(ctx context.Context, id int64) (*Asset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// DeleteAsset deletes an asset row.
func (s *Store) DeleteAsset(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM assets WHERE id=?`, id)
	return affected(res, err)
}

// CountDownload increments the download counter.
func (s *Store) CountDownload(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE assets SET downloads=downloads+1 WHERE id=?`, id)
	return err
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReleaseStatus loads the status of the release with the given id.
func (s *Store) ReleaseStatus(ctx context.Context, releaseID int64, status *string) error {
	err := s.db.QueryRowContext(ctx, `SELECT status FROM releases WHERE id=?`, releaseID).Scan(status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
