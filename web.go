package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// logRequests logs HTTP requests to the given (stderr) logger.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start).String())
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

//go:embed index.html
var staticFS embed.FS

// HTTPHandler wires the web UI and JSON API. filesRoot is the absolute,
// symlink-resolved directory exposed by the file browser endpoints.
func (p *Phone) HTTPHandler(filesRoot string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, err := staticFS.ReadFile("index.html")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Server   string `json:"server"`
			Number   string `json:"number"`
			Password string `json:"password"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := p.Register(strings.TrimSpace(body.Server), strings.TrimSpace(body.Number), body.Password); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/unregister", func(w http.ResponseWriter, r *http.Request) {
		p.Unregister()
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/call", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Number string `json:"number"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := p.Call(strings.TrimSpace(body.Number)); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := p.CancelCall(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/answer", func(w http.ResponseWriter, r *http.Request) {
		if err := p.Answer(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/decline", func(w http.ResponseWriter, r *http.Request) {
		if err := p.Decline(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p.Status())
	})

	mux.HandleFunc("POST /api/hangup", func(w http.ResponseWriter, r *http.Request) {
		if err := p.Hangup(); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, p.Status())
	})

	// File browser: lazy per-directory listing, one level per request.
	mux.HandleFunc("GET /api/fs/tree", func(w http.ResponseWriter, r *http.Request) {
		rel := fsCleanPath(r.URL.Query().Get("path"))
		full, err := fsResolve(filesRoot, rel)
		if err != nil {
			fsError(w, err)
			return
		}
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
			writeError(w, http.StatusBadRequest, errors.New("это файл, а не каталог"))
			return
		}
		items, err := os.ReadDir(full)
		if err != nil {
			fsError(w, err)
			return
		}
		var entries []fsEntry
		truncated := false
		for _, d := range items {
			if fsIgnore[d.Name()] {
				continue
			}
			if len(entries) >= fsMaxEntries {
				truncated = true
				break
			}
			en := fsEntry{Name: d.Name(), Path: fsJoin(rel, d.Name()), Dir: d.IsDir()}
			if !en.Dir {
				if fi, err := d.Info(); err == nil {
					en.Size = fi.Size()
				}
			}
			entries = append(entries, en)
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Dir != entries[j].Dir {
				return entries[i].Dir
			}
			return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"path": rel, "truncated": truncated, "entries": entries,
		})
	})

	// File preview: text contents only, size-capped.
	mux.HandleFunc("GET /api/fs/file", func(w http.ResponseWriter, r *http.Request) {
		rel := fsCleanPath(r.URL.Query().Get("path"))
		full, err := fsResolve(filesRoot, rel)
		if err != nil {
			fsError(w, err)
			return
		}
		fi, err := os.Stat(full)
		if err != nil {
			fsError(w, err)
			return
		}
		if fi.IsDir() {
			writeError(w, http.StatusBadRequest, errors.New("это каталог, а не файл"))
			return
		}
		f, err := os.Open(full)
		if err != nil {
			fsError(w, err)
			return
		}
		defer f.Close()

		out := fsFile{Path: rel, Size: fi.Size(), ModTime: fi.ModTime().Format(time.RFC3339)}
		switch {
		case fi.Size() > fsMaxTextSize:
			out.Kind = "too_large"
		default:
			head := make([]byte, fsPeekSize)
			n, err := io.ReadFull(f, head)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				writeError(w, http.StatusInternalServerError, errors.New("внутренняя ошибка"))
				return
			}
			if bytes.ContainsRune(head[:n], 0) {
				out.Kind = "binary"
			} else {
				data, err := io.ReadAll(f)
				if err != nil {
					writeError(w, http.StatusInternalServerError, errors.New("внутренняя ошибка"))
					return
				}
				out.Kind = "text"
				out.Text = string(head[:n]) + string(data)
			}
		}
		writeJSON(w, http.StatusOK, out)
	})

	return logRequests(p.log, mux)
}

// File browser limits and filters.
const (
	fsMaxEntries  = 500
	fsMaxTextSize = 512 << 10
	fsPeekSize    = 8 << 10
)

var fsIgnore = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true}

type fsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"` // slash-separated, relative to the root
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

type fsFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
	Kind    string `json:"kind"` // "text" | "binary" | "too_large"
	Text    string `json:"text,omitempty"`
}

// fsCleanPath normalizes a root-relative request path to slash form without
// leading/trailing separators; the "/" prefix in Clean kills every "..".
func fsCleanPath(rel string) string {
	return strings.Trim(filepath.ToSlash(filepath.Clean("/"+rel)), "/")
}

func fsJoin(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}

var errFsEscape = errors.New("путь за пределами корневого каталога")

// fsResolve maps a root-relative path to a real filesystem path, re-checking
// containment after symlink resolution so nothing can escape the root.
func fsResolve(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+rel)))
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err // carries os.ErrNotExist for the 404 mapping
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", errFsEscape
	}
	return real, nil
}

func fsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, errFsEscape):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, os.ErrPermission):
		writeError(w, http.StatusForbidden, err)
	default:
		writeError(w, http.StatusInternalServerError, errors.New("внутренняя ошибка"))
	}
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
