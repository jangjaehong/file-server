package server

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jangjaehong/file-server/internal/semver"
	"github.com/jangjaehong/file-server/internal/store"
)

func (s *Server) handleListPrograms(w http.ResponseWriter, r *http.Request) {
	progs, err := s.st.ListPrograms(r.Context(), false)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, progs)
}

// programDetail is the public view of a program.
type programDetail struct {
	*store.Program
	Latest     *store.Release  `json:"latest"`
	LatestBeta *store.Release  `json:"latest_beta,omitempty"`
	Releases   []store.Release `json:"releases"`
}

func (s *Server) handleGetProgram(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	rels, err := s.st.ListReleases(r.Context(), p.ID, false)
	if err != nil {
		storeErr(w, err)
		return
	}
	d := programDetail{Program: p, Releases: rels, Latest: store.Latest(rels, store.ChannelStable)}
	if b := store.Latest(rels, store.ChannelBeta); b != nil && (d.Latest == nil || b.ID != d.Latest.ID) {
		d.LatestBeta = b
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	rels, err := s.st.ListReleases(r.Context(), p.ID, false)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	rel, _, ok := s.resolveRelease(w, r)
	if ok {
		writeJSON(w, http.StatusOK, rel)
	}
}

// resolveRelease finds the published release for {slug}/{version}.
// version may be "latest" (optionally ?channel=beta).
func (s *Server) resolveRelease(w http.ResponseWriter, r *http.Request) (*store.Release, *store.Program, bool) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return nil, nil, false
	}
	ver := r.PathValue("version")
	if ver == "latest" {
		rels, err := s.st.ListReleases(r.Context(), p.ID, false)
		if err != nil {
			storeErr(w, err)
			return nil, nil, false
		}
		rel := store.Latest(rels, channelParam(r))
		if rel == nil {
			writeErr(w, http.StatusNotFound, "no published release")
			return nil, nil, false
		}
		return rel, p, true
	}
	v, err := semver.Parse(ver)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	rel, err := s.st.GetRelease(r.Context(), p.ID, canonical(v))
	if err != nil {
		storeErr(w, err)
		return nil, nil, false
	}
	if rel.Status != store.StatusPublished {
		if _, isAdmin := s.authenticate(r); !isAdmin {
			writeErr(w, http.StatusNotFound, "not found")
			return nil, nil, false
		}
	}
	return rel, p, true
}

func channelParam(r *http.Request) string {
	if r.URL.Query().Get("channel") == store.ChannelBeta {
		return store.ChannelBeta
	}
	return store.ChannelStable
}

// canonical returns the stored form of a version (build metadata dropped).
func canonical(v semver.Version) string {
	v.Build = ""
	return v.String()
}

// pickAsset chooses the asset for a platform, falling back to "any".
func pickAsset(rel *store.Release, platform string) *store.Asset {
	var fallback *store.Asset
	for i := range rel.Assets {
		a := &rel.Assets[i]
		if a.Platform == platform {
			return a
		}
		if a.Platform == "any" && fallback == nil {
			fallback = a
		}
	}
	return fallback
}

// ---------------------------------------------------------------- downloads

func (s *Server) handleDownloadByPlatform(w http.ResponseWriter, r *http.Request) {
	rel, _, ok := s.resolveRelease(w, r)
	if !ok {
		return
	}
	a := pickAsset(rel, r.PathValue("platform"))
	if a == nil {
		writeErr(w, http.StatusNotFound, "no file for this platform")
		return
	}
	s.serveAsset(w, r, a)
}

func (s *Server) handleDownloadByName(w http.ResponseWriter, r *http.Request) {
	rel, _, ok := s.resolveRelease(w, r)
	if !ok {
		return
	}
	for i := range rel.Assets {
		a := &rel.Assets[i]
		if a.Platform == r.PathValue("platform") && a.Filename == r.PathValue("filename") {
			s.serveAsset(w, r, a)
			return
		}
	}
	writeErr(w, http.StatusNotFound, "not found")
}

func (s *Server) handleDownloadByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := s.st.GetAsset(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	// Only files of published releases are public.
	if !s.assetPublic(r, a) {
		if _, isAdmin := s.authenticate(r); !isAdmin {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
	}
	s.serveAsset(w, r, a)
}

func (s *Server) assetPublic(r *http.Request, a *store.Asset) bool {
	var status string
	err := s.st.ReleaseStatus(r.Context(), a.ReleaseID, &status)
	return err == nil && status == store.StatusPublished
}

// serveAsset streams a file. Supports Range (resumable downloads) and ETag.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, a *store.Asset) {
	f, err := os.Open(filepath.Join(s.cfg.DataDir, "files", filepath.FromSlash(a.StoragePath)))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file missing on server")
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiFallback(a.Filename), url.PathEscape(a.Filename)))
	h.Set("ETag", `"`+a.SHA256+`"`)
	h.Set("X-Checksum-SHA256", a.SHA256)
	h.Set("Cache-Control", "public, max-age=3600")
	rg := r.Header.Get("Range")
	if r.Method == http.MethodGet && (rg == "" || strings.HasPrefix(rg, "bytes=0-")) && r.Header.Get("If-None-Match") == "" {
		_ = s.st.CountDownload(r.Context(), a.ID)
	}
	http.ServeContent(w, r, "", a.CreatedAt, f)
}

func asciiFallback(name string) string {
	var b strings.Builder
	for _, c := range name {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			b.WriteByte('_')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- auto update

// UpdateResponse is returned by GET /api/v1/update.
type UpdateResponse struct {
	Program         string        `json:"program"`
	CurrentVersion  string        `json:"current_version"`
	UpdateAvailable bool          `json:"update_available"`
	Mandatory       bool          `json:"mandatory"` // a mandatory release lies between current and latest
	CurrentYanked   bool          `json:"current_yanked"`
	Latest          *UpdateTarget `json:"latest,omitempty"`
	Changes         []ChangeEntry `json:"changes"` // patch notes of all skipped versions, newest first
}

// UpdateTarget describes the file the client should install.
type UpdateTarget struct {
	Version     string     `json:"version"`
	Channel     string     `json:"channel"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Platform    string     `json:"platform"`
	Filename    string     `json:"filename"`
	Size        int64      `json:"size"`
	SHA256      string     `json:"sha256"`
	URL         string     `json:"url"`
}

// ChangeEntry is a patch-notes entry.
type ChangeEntry struct {
	Version     string     `json:"version"`
	Mandatory   bool       `json:"mandatory"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Notes       string     `json:"notes"`
}

// handleUpdateCheck: GET /api/v1/update?program=A&version=1.2.0&platform=windows-amd64&channel=stable
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slug, platform := q.Get("program"), q.Get("platform")
	if slug == "" || platform == "" || q.Get("version") == "" {
		writeErr(w, http.StatusBadRequest, "program, version and platform are required")
		return
	}
	cur, err := semver.Parse(q.Get("version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.st.GetProgram(r.Context(), slug)
	if err != nil {
		storeErr(w, err)
		return
	}
	all, err := s.st.ListReleases(r.Context(), p.ID, true) // newest first
	if err != nil {
		storeErr(w, err)
		return
	}
	channel := channelParam(r)
	resp := UpdateResponse{Program: p.Slug, CurrentVersion: cur.String(), Changes: []ChangeEntry{}}

	var target *store.Release
	var asset *store.Asset
	for i := range all {
		rel := &all[i]
		if rel.Version == canonical(cur) && rel.Status == store.StatusYanked {
			resp.CurrentYanked = true
		}
		if rel.Status != store.StatusPublished || (rel.Channel == store.ChannelBeta && channel != store.ChannelBeta) {
			continue
		}
		if target == nil {
			if a := pickAsset(rel, platform); a != nil {
				target, asset = rel, a
			}
		}
	}
	if target != nil {
		tv := semver.MustParse(target.Version)
		if semver.Less(cur, tv) || resp.CurrentYanked && semver.Compare(cur, tv) != 0 {
			resp.UpdateAvailable = true
			resp.Latest = &UpdateTarget{
				Version: target.Version, Channel: target.Channel, PublishedAt: target.PublishedAt,
				Platform: asset.Platform, Filename: asset.Filename, Size: asset.Size, SHA256: asset.SHA256,
				URL: s.baseURL(r) + "/files/" + strconv.FormatInt(asset.ID, 10),
			}
			for i := range all {
				rel := &all[i]
				if rel.Status != store.StatusPublished || (rel.Channel == store.ChannelBeta && channel != store.ChannelBeta) {
					continue
				}
				v := semver.MustParse(rel.Version)
				if semver.Less(cur, v) && semver.Compare(v, tv) <= 0 {
					resp.Changes = append(resp.Changes, ChangeEntry{Version: rel.Version, Mandatory: rel.Mandatory, PublishedAt: rel.PublishedAt, Notes: rel.Notes})
					if rel.Mandatory {
						resp.Mandatory = true
					}
				}
			}
			if resp.CurrentYanked {
				resp.Mandatory = true
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}
