package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/jangjaehong/file-server/internal/semver"
	"github.com/jangjaehong/file-server/internal/store"
)

// ---------------------------------------------------------------- programs

type programInput struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
}

func (s *Server) handleAdminListPrograms(w http.ResponseWriter, r *http.Request) {
	progs, err := s.st.ListPrograms(r.Context(), true)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, progs)
}

func (s *Server) handleCreateProgram(w http.ResponseWriter, r *http.Request) {
	var in programInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if !slugRe.MatchString(in.Slug) {
		writeErr(w, http.StatusBadRequest, "slug must be lowercase letters, digits and '-' (e.g. my-app)")
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	p := &store.Program{Slug: in.Slug, Name: strings.TrimSpace(in.Name), Summary: in.Summary, Description: in.Description}
	if err := s.st.CreateProgram(r.Context(), p); err != nil {
		storeErr(w, err)
		return
	}
	log.Printf("%s created program %s", actor(r), p.Slug)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleUpdateProgram(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	var in programInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	p.Name, p.Summary, p.Description = strings.TrimSpace(in.Name), in.Summary, in.Description
	if err := s.st.UpdateProgram(r.Context(), p); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProgram(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := s.st.DeleteProgram(r.Context(), p.ID); err != nil {
		storeErr(w, err)
		return
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.DataDir, "files", p.Slug))
	log.Printf("%s deleted program %s", actor(r), p.Slug)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- releases

type releaseInput struct {
	Version   string `json:"version"`
	Channel   string `json:"channel"`
	Notes     string `json:"notes"`
	Mandatory bool   `json:"mandatory"`
}

func validChannel(c string) (string, bool) {
	switch c {
	case "", store.ChannelStable:
		return store.ChannelStable, true
	case store.ChannelBeta:
		return store.ChannelBeta, true
	}
	return "", false
}

func (s *Server) handleAdminListReleases(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	rels, err := s.st.ListReleases(r.Context(), p.ID, true)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	var in releaseInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	v, err := semver.Parse(in.Version)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch, ok := validChannel(in.Channel)
	if !ok {
		writeErr(w, http.StatusBadRequest, "channel must be stable or beta")
		return
	}
	existing, err := s.st.ListReleases(r.Context(), p.ID, true)
	if err != nil {
		storeErr(w, err)
		return
	}
	for _, e := range existing {
		if ev, err := semver.Parse(e.Version); err == nil && semver.Compare(ev, v) == 0 {
			writeErr(w, http.StatusConflict, "version "+e.Version+" already exists; versions are immutable, use a new version number")
			return
		}
	}
	rel := &store.Release{ProgramID: p.ID, Version: canonical(v), Channel: ch, Notes: in.Notes, Mandatory: in.Mandatory}
	if err := s.st.CreateRelease(r.Context(), rel); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "version "+rel.Version+" already exists; versions are immutable, use a new version number")
			return
		}
		storeErr(w, err)
		return
	}
	log.Printf("%s created release %s %s", actor(r), p.Slug, rel.Version)
	writeJSON(w, http.StatusCreated, rel)
}

// loadRelease loads {slug}/{version} for admin endpoints (any status).
func (s *Server) loadRelease(w http.ResponseWriter, r *http.Request) (*store.Program, *store.Release, bool) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return nil, nil, false
	}
	v, err := semver.Parse(r.PathValue("version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	rel, err := s.st.GetRelease(r.Context(), p.ID, canonical(v))
	if err != nil {
		storeErr(w, err)
		return nil, nil, false
	}
	return p, rel, true
}

// handleAdminGetRelease returns a release in any status (used by publish tools to resume or skip).
func (s *Server) handleAdminGetRelease(w http.ResponseWriter, r *http.Request) {
	if _, rel, ok := s.loadRelease(w, r); ok {
		writeJSON(w, http.StatusOK, rel)
	}
}

// handleUpdateRelease edits notes/channel/mandatory. The version number and the
// files of a published release can never change.
func (s *Server) handleUpdateRelease(w http.ResponseWriter, r *http.Request) {
	_, rel, ok := s.loadRelease(w, r)
	if !ok {
		return
	}
	var in releaseInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if in.Version != "" && in.Version != rel.Version {
		writeErr(w, http.StatusBadRequest, "version number cannot be changed")
		return
	}
	ch, valid := validChannel(in.Channel)
	if !valid {
		writeErr(w, http.StatusBadRequest, "channel must be stable or beta")
		return
	}
	rel.Channel, rel.Notes, rel.Mandatory = ch, in.Notes, in.Mandatory
	if err := s.st.UpdateRelease(r.Context(), rel); err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Server) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	p, rel, ok := s.loadRelease(w, r)
	if !ok {
		return
	}
	if rel.Status != store.StatusDraft {
		writeErr(w, http.StatusConflict, "only draft releases can be deleted; yank a published release instead")
		return
	}
	if err := s.st.DeleteRelease(r.Context(), rel.ID); err != nil {
		storeErr(w, err)
		return
	}
	_ = os.RemoveAll(filepath.Join(s.cfg.DataDir, "files", p.Slug, rel.Version))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, rel, ok := s.loadRelease(w, r)
		if !ok {
			return
		}
		if status == store.StatusPublished && len(rel.Assets) == 0 {
			writeErr(w, http.StatusConflict, "upload at least one file before publishing")
			return
		}
		if err := s.st.SetReleaseStatus(r.Context(), rel.ID, status); err != nil {
			storeErr(w, err)
			return
		}
		log.Printf("%s set %s %s -> %s", actor(r), p.Slug, rel.Version, status)
		rel, err := s.st.GetRelease(r.Context(), p.ID, rel.Version)
		if err != nil {
			storeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rel)
	}
}

// ---------------------------------------------------------------- assets

// sanitizeFilename keeps only the base name and rejects dangerous names.
func sanitizeFilename(name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" || strings.HasPrefix(name, ".") || len(name) > 200 {
		return "", false
	}
	for _, c := range name {
		if unicode.IsControl(c) || strings.ContainsRune(`<>:"|?*`, c) {
			return "", false
		}
	}
	return name, true
}

// handleUploadAsset accepts multipart/form-data with fields:
//
//	platform  e.g. windows-amd64 (may also be given as ?platform=), must precede the file part
//	file      one or more files
//
// Files are streamed to disk (no size held in memory) and hashed with SHA-256.
// Files can only be added while the release is a draft.
func (s *Server) handleUploadAsset(w http.ResponseWriter, r *http.Request) {
	p, rel, ok := s.loadRelease(w, r)
	if !ok {
		return
	}
	if rel.Status != store.StatusDraft {
		writeErr(w, http.StatusConflict, "files of a published release are immutable; create a new version")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "multipart/form-data expected")
		return
	}
	platform := strings.ToLower(r.URL.Query().Get("platform"))
	var added []store.Asset
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "malformed upload: "+err.Error())
			return
		}
		switch part.FormName() {
		case "platform":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			platform = strings.ToLower(strings.TrimSpace(string(b)))
		case "file":
			if !platformRe.MatchString(platform) {
				part.Close()
				writeErr(w, http.StatusBadRequest, "valid platform required before file (e.g. windows-amd64, linux-amd64, any)")
				return
			}
			a, code, err := s.saveAsset(r.Context(), p, rel, platform, part)
			part.Close()
			if err != nil {
				writeErr(w, code, err.Error())
				return
			}
			added = append(added, *a)
			log.Printf("%s uploaded %s %s %s/%s (%d bytes)", actor(r), p.Slug, rel.Version, platform, a.Filename, a.Size)
		default:
			part.Close()
		}
	}
	if len(added) == 0 {
		writeErr(w, http.StatusBadRequest, "no file in upload")
		return
	}
	writeJSON(w, http.StatusCreated, added)
}

func (s *Server) saveAsset(ctx context.Context, p *store.Program, rel *store.Release, platform string, part *multipart.Part) (*store.Asset, int, error) {
	name, ok := sanitizeFilename(part.FileName())
	if !ok {
		return nil, http.StatusBadRequest, errors.New("invalid file name")
	}
	up, code, err := s.receiveFile(part)
	if err != nil {
		return nil, code, err
	}
	defer os.Remove(up.tmp) // no-op after successful rename
	n := up.size
	relPath := path.Join(p.Slug, rel.Version, platform, name)
	dst := filepath.Join(s.cfg.DataDir, "files", filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if _, err := os.Stat(dst); err == nil {
		return nil, http.StatusConflict, errors.New(platform + "/" + name + " already uploaded for this version; delete it first")
	}
	a := &store.Asset{ReleaseID: rel.ID, Platform: platform, Filename: name, Size: n,
		SHA256: up.sha256, StoragePath: relPath}
	if err := s.st.AddAsset(ctx, a); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, http.StatusConflict, errors.New(platform + "/" + name + " already uploaded for this version")
		}
		return nil, http.StatusInternalServerError, err
	}
	if err := os.Rename(up.tmp, dst); err != nil {
		_ = s.st.DeleteAsset(ctx, a.ID)
		return nil, http.StatusInternalServerError, err
	}
	return a, 0, nil
}

type received struct {
	tmp    string
	size   int64
	sha256 string
}

// receiveFile streams an uploaded part into data/tmp while hashing it.
// The caller must remove (or rename) r.tmp.
func (s *Server) receiveFile(part io.Reader) (*received, int, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.cfg.DataDir, "tmp"), "upload-*")
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(part, s.cfg.MaxUploadSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	fail := func(code int, e error) (*received, int, error) {
		os.Remove(tmp.Name())
		return nil, code, e
	}
	switch {
	case err != nil:
		return fail(http.StatusBadRequest, errors.New("upload interrupted: "+err.Error()))
	case n > s.cfg.MaxUploadSize:
		return fail(http.StatusRequestEntityTooLarge, errors.New("file too large"))
	case n == 0:
		return fail(http.StatusBadRequest, errors.New("empty file"))
	}
	return &received{tmp: tmp.Name(), size: n, sha256: hex.EncodeToString(h.Sum(nil))}, 0, nil
}

func (s *Server) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := s.st.GetAsset(r.Context(), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	var status string
	if err := s.st.ReleaseStatus(r.Context(), a.ReleaseID, &status); err != nil {
		storeErr(w, err)
		return
	}
	if status != store.StatusDraft {
		writeErr(w, http.StatusConflict, "files of a published release are immutable")
		return
	}
	if err := s.st.DeleteAsset(r.Context(), id); err != nil {
		storeErr(w, err)
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.DataDir, "files", filepath.FromSlash(a.StoragePath)))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
