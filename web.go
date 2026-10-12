package main

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
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

// HTTPHandler wires the web UI and JSON API.
func (p *Phone) HTTPHandler() http.Handler {
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

	return logRequests(p.log, mux)
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
