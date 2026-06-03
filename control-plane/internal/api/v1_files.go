package api

import (
	"archive/zip"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File reads are served by sandboxd directly from the host-side
// workspace loopback mount (ops/design/v1-external-api.md §2) — so
// they work whether or not the sandbox is running, and runtimed is
// not on the path.

const (
	appSubdir    = "workspace/app"
	maxFileBytes = 2 << 20 // 2 MiB cap on a single file read
)

// excludedFromFiles are never listed, read, or exported.
var excludedFromFiles = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, ".vite": true,
}

func (s *Server) appDirFor(id string) string {
	_, mnt := s.Loopback.Paths(id)
	return filepath.Join(mnt, appSubdir)
}

// safeJoin resolves a caller-supplied path under root, rejecting any
// escape (`..`, absolute paths).
func safeJoin(root, p string) (string, bool) {
	full := filepath.Join(root, filepath.Clean("/"+p))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

type fileEntry struct {
	Path string `json:"path"` // relative to the app dir
	Type string `json:"type"` // "file" | "dir"
	Size int64  `json:"size,omitempty"`
}

// --- GET /v1/sandboxes/{id}/files -----------------------------------

func (s *Server) v1ListFiles(w http.ResponseWriter, r *http.Request) {
	root := s.appDirFor(r.PathValue("id"))
	p := r.URL.Query().Get("path")
	recursive := r.URL.Query().Get("recursive") == "true"
	dir, ok := safeJoin(root, p)
	if !ok {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}

	var entries []fileEntry
	add := func(path string, d fs.DirEntry) {
		rel, _ := filepath.Rel(root, path)
		e := fileEntry{Path: rel, Type: "file"}
		if d.IsDir() {
			e.Type = "dir"
		} else if fi, err := d.Info(); err == nil {
			e.Size = fi.Size()
		}
		entries = append(entries, e)
	}
	if recursive {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path == dir {
				return nil
			}
			if excludedFromFiles[d.Name()] {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			add(path, d)
			return nil
		})
	} else {
		ents, _ := os.ReadDir(dir)
		for _, d := range ents {
			if excludedFromFiles[d.Name()] {
				continue
			}
			add(filepath.Join(dir, d.Name()), d)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	writeJSON(w, http.StatusOK, map[string]any{
		"path": p, "recursive": recursive, "entries": entries,
	})
}

// --- GET /v1/sandboxes/{id}/files/content ---------------------------

func (s *Server) v1FileContent(w http.ResponseWriter, r *http.Request) {
	root := s.appDirFor(r.PathValue("id"))
	full, ok := safeJoin(root, r.URL.Query().Get("path"))
	if !ok || full == root {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if info.Size() > maxFileBytes {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "file exceeds the 2 MiB read cap")
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

// --- GET /v1/sandboxes/{id}/export ----------------------------------

func (s *Server) v1Export(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	root := s.appDirFor(id)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no workspace for that sandbox")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		if excludedFromFiles[d.Name()] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fw, werr := zw.Create(rel)
		if werr != nil {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return nil
		}
		defer f.Close()
		_, _ = io.Copy(fw, f)
		return nil
	})
}
