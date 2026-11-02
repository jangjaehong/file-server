package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jangjaehong/file-server/internal/semver"
	"github.com/jangjaehong/file-server/internal/store"
)

// Static update layout ("latest.json" scheme)
//
// Existing Windows programs (One TaskBar, ICS-SCM) update from a plain web folder:
//
//	http://host:9100/<program>/latest.json   {"version","file","sha256","notes"}
//	http://host:9100/<program>/<file>        the exe named by latest.json
//	http://host:9100/<program>/defaults.json other fixed files next to it
//
// The server answers exactly these URLs for every program slug, so such clients
// work unchanged once their update address points here.

// compatName is the file name a release asset gets in the static layout. It must be
// unique per version, so the version is added when the uploaded name lacks it
// (OneTaskBar-Setup.exe -> OneTaskBar-Setup-1.0.0.57.exe).
func compatName(rel *store.Release, a *store.Asset) string {
	if strings.Contains(a.Filename, rel.Version) {
		return a.Filename
	}
	ext := path.Ext(a.Filename)
	return strings.TrimSuffix(a.Filename, ext) + "-" + rel.Version + ext
}

// compatAsset picks the Windows executable of a release for latest.json.
func compatAsset(rel *store.Release, platform string) *store.Asset {
	prefs := []string{platform, "windows-amd64", "windows-x64", "windows", "any"}
	for _, p := range prefs {
		for i := range rel.Assets {
			a := &rel.Assets[i]
			if p != "" && a.Platform == p && strings.HasSuffix(strings.ToLower(a.Filename), ".exe") {
				return a
			}
		}
	}
	for i := range rel.Assets {
		if strings.HasSuffix(strings.ToLower(rel.Assets[i].Filename), ".exe") {
			return &rel.Assets[i]
		}
	}
	return nil
}

// compatManifest is the latest.json body. Extra fields are ignored by old clients.
type compatManifest struct {
	Version     string     `json:"version"`
	File        string     `json:"file"`
	SHA256      string     `json:"sha256"`
	Notes       string     `json:"notes"`
	Mandatory   bool       `json:"mandatory"`
	Size        int64      `json:"size"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// handleStatic serves GET /{slug}/{name}: latest.json, release executables and fixed files.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	slug, name := r.PathValue("slug"), r.PathValue("name")
	if !slugRe.MatchString(slug) {
		http.NotFound(w, r)
		return
	}
	p, err := s.st.GetProgram(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rels, err := s.st.ListReleases(r.Context(), p.ID, false) // published, newest first
	if err != nil {
		storeErr(w, err)
		return
	}

	if strings.EqualFold(name, "latest.json") {
		// Only stable, purely numeric versions: .NET's Version.TryParse rejects "-beta".
		channel := channelParam(r)
		for i := range rels {
			rel := &rels[i]
			v, err := semver.Parse(rel.Version)
			if err != nil || !v.IsNumeric() || (rel.Channel != store.ChannelStable && channel != store.ChannelBeta) {
				continue
			}
			a := compatAsset(rel, r.URL.Query().Get("platform"))
			if a == nil {
				continue
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, compatManifest{
				Version: rel.Version, File: compatName(rel, a), SHA256: a.SHA256, Notes: rel.Notes,
				Mandatory: rel.Mandatory, Size: a.Size, PublishedAt: rel.PublishedAt,
			})
			return
		}
		writeErr(w, http.StatusNotFound, "no published Windows release")
		return
	}

	for i := range rels {
		for j := range rels[i].Assets {
			a := &rels[i].Assets[j]
			if strings.EqualFold(compatName(&rels[i], a), name) {
				s.serveAsset(w, r, a)
				return
			}
		}
	}
	if f, err := s.st.GetProgramFile(r.Context(), p.ID, name); err == nil {
		s.serveProgramFile(w, r, f)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) serveProgramFile(w http.ResponseWriter, r *http.Request, f *store.ProgramFile) {
	fh, err := os.Open(filepath.Join(s.cfg.DataDir, "files", filepath.FromSlash(f.StoragePath)))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file missing on server")
		return
	}
	defer fh.Close()
	h := w.Header()
	if strings.HasSuffix(strings.ToLower(f.Name), ".json") {
		h.Set("Content-Type", "application/json; charset=utf-8")
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("ETag", `"`+f.SHA256+`"`)
	h.Set("X-Checksum-SHA256", f.SHA256)
	h.Set("Cache-Control", "no-cache") // contents can be replaced at any time
	if r.Method == http.MethodGet && r.Header.Get("Range") == "" && r.Header.Get("If-None-Match") == "" {
		_ = s.st.CountProgramFileDownload(r.Context(), f.ID)
	}
	http.ServeContent(w, r, "", f.UpdatedAt, fh)
}

// ---------------------------------------------------------------- admin: fixed files

func (s *Server) handleListProgramFiles(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	files, err := s.st.ListProgramFiles(r.Context(), p.ID)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// handleUploadProgramFiles: multipart "file" parts; an existing file with the same name is replaced.
func (s *Server) handleUploadProgramFiles(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "multipart/form-data expected")
		return
	}
	var saved []store.ProgramFile
	for {
		part, err := mr.NextPart()
		if err != nil {
			if err != io.EOF {
				writeErr(w, http.StatusBadRequest, "malformed upload: "+err.Error())
				return
			}
			break
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		name, ok := sanitizeFilename(part.FileName())
		if !ok || strings.EqualFold(name, "latest.json") {
			part.Close()
			writeErr(w, http.StatusBadRequest, "invalid file name (latest.json is generated by the server)")
			return
		}
		f, code, err := s.saveProgramFile(r, p, name, part)
		part.Close()
		if err != nil {
			writeErr(w, code, err.Error())
			return
		}
		saved = append(saved, *f)
		log.Printf("%s uploaded fixed file %s/%s (%d bytes)", actor(r), p.Slug, name, f.Size)
	}
	if len(saved) == 0 {
		writeErr(w, http.StatusBadRequest, "no file in upload")
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) saveProgramFile(r *http.Request, p *store.Program, name string, part io.Reader) (*store.ProgramFile, int, error) {
	up, code, err := s.receiveFile(part)
	if err != nil {
		return nil, code, err
	}
	defer os.Remove(up.tmp)
	relPath := path.Join(p.Slug, "_files", name) // "_files" can never be a version directory
	dst := filepath.Join(s.cfg.DataDir, "files", filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := os.Rename(up.tmp, dst); err != nil { // atomic replace
		return nil, http.StatusInternalServerError, err
	}
	f := &store.ProgramFile{ProgramID: p.ID, Name: name, Size: up.size, SHA256: up.sha256, StoragePath: relPath}
	if err := s.st.PutProgramFile(r.Context(), f); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return f, 0, nil
}

func (s *Server) handleDeleteProgramFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.st.GetProgram(r.Context(), r.PathValue("slug"))
	if err != nil {
		storeErr(w, err)
		return
	}
	f, err := s.st.GetProgramFile(r.Context(), p.ID, r.PathValue("name"))
	if err != nil {
		storeErr(w, err)
		return
	}
	if err := s.st.DeleteProgramFile(r.Context(), f.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		storeErr(w, err)
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.DataDir, "files", filepath.FromSlash(f.StoragePath)))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
