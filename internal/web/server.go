/*
Copyright 2026 Bitasylum

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package web serves the stub's page and its JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"github.com/bitasylum/mode-operator/client/clustermode"

	"github.com/bitasylum/mode-manager-stub/internal/manager"
)

//go:embed static
var static embed.FS

// Manager is what the server needs from the Manager.
type Manager interface {
	Snapshot(ctx context.Context) manager.Snapshot
	SetMode(ctx context.Context, mode string, check bool) error
	MoveTo(ctx context.Context, target string) error
	StopMove() bool
}

// Result is the response to a change.
type Result struct {
	OK bool `json:"ok"`
	// Reason is the client library's reason, when the change failed.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Handler returns the server's handler. Changes started from a request run
// with base, so a move outlives the request that started it.
func Handler(base context.Context, m Manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", file("static/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", file("static/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", file("static/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Snapshot(r.Context()))
	})
	mux.HandleFunc("POST /api/mode", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mode  string `json:"mode"`
			Check bool   `json:"check"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		err := m.SetMode(r.Context(), req.Mode, req.Check)
		writeJSON(w, status(err), result(err))
	})
	mux.HandleFunc("POST /api/move", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target string `json:"target"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		err := m.MoveTo(base, req.Target)
		writeJSON(w, status(err), result(err))
	})
	mux.HandleFunc("DELETE /api/move", func(w http.ResponseWriter, _ *http.Request) {
		if !m.StopMove() {
			writeJSON(w, http.StatusConflict, Result{Message: "no move is running"})
			return
		}
		writeJSON(w, http.StatusOK, Result{OK: true})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// The page has no login, so a page on another site mustn't be able to
	// post to it through a tester's port-forward. Requests from the page
	// itself, and from programs, are same-origin or have no origin.
	return secure(http.NewCrossOriginProtection().Handler(mux))
}

// status is the HTTP status for the result of a change. A change the API
// server or the rules refuse is answered with 200 and the reason.
func status(err error) int {
	switch {
	case errors.Is(err, manager.ErrBusy), errors.Is(err, manager.ErrNotReady):
		return http.StatusConflict
	case errors.Is(err, manager.ErrInvalidMode):
		return http.StatusBadRequest
	}
	return http.StatusOK
}

func result(err error) Result {
	if err == nil {
		return Result{OK: true}
	}
	return Result{Reason: string(clustermode.ReasonOf(err)), Message: err.Error()}
}

func file(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data, err := static.ReadFile(name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}
}

// maxBody is the largest request body accepted.
const maxBody = 4096

// readJSON reads a JSON request body. Requiring application/json also means
// a browser sends a preflight before a cross-origin request.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, Result{Message: "the request must be application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeJSON(w, http.StatusRequestEntityTooLarge, Result{Message: "the request is too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, Result{Message: "malformed request: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// secure adds headers that keep the page from loading anything but its own
// files, or being framed.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
