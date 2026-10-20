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

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bitasylum/mode-operator/client/clustermode"

	"github.com/bitasylum/mode-manager-stub/internal/manager"
)

type fakeManager struct {
	mode    string
	check   bool
	target  string
	setErr  error
	moveErr error
	moving  bool
}

func (f *fakeManager) Snapshot(context.Context) manager.Snapshot {
	return manager.Snapshot{Phase: manager.PhaseRunning, Mode: "normal", Next: []string{"peak"}}
}

func (f *fakeManager) SetMode(_ context.Context, mode string, check bool) error {
	f.mode, f.check = mode, check
	return f.setErr
}

func (f *fakeManager) MoveTo(_ context.Context, target string) error {
	f.target = target
	return f.moveErr
}

func (f *fakeManager) StopMove() bool {
	stopped := f.moving
	f.moving = false
	return stopped
}

// do sends a request as the page does: a POST has a JSON body.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return send(h, req)
}

func send(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) Result {
	t.Helper()
	var r Result
	if err := json.NewDecoder(rec.Body).Decode(&r); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return r
}

func TestState(t *testing.T) {
	h := Handler(t.Context(), &fakeManager{})
	rec := do(t, h, http.MethodGet, "/api/state", "")
	var s manager.Snapshot
	if err := json.NewDecoder(rec.Body).Decode(&s); err != nil || s.Mode != "normal" || s.Phase != manager.PhaseRunning {
		t.Errorf("state = %+v, %v", s, err)
	}
}

func TestSetMode(t *testing.T) {
	m := &fakeManager{}
	h := Handler(t.Context(), m)
	if r := decode(t, do(t, h, http.MethodPost, "/api/mode", `{"mode":"peak","check":true}`)); !r.OK {
		t.Errorf("result = %+v", r)
	}
	if m.mode != "peak" || !m.check {
		t.Errorf("SetMode got %q, check %v", m.mode, m.check)
	}

	m.setErr = &clustermode.Error{Reason: clustermode.ReasonTransitionNotAllowed, From: "peak", Mode: "offpeak"}
	r := decode(t, do(t, h, http.MethodPost, "/api/mode", `{"mode":"offpeak"}`))
	if r.OK || r.Reason != "TransitionNotAllowed" || r.Message == "" {
		t.Errorf("a refused change = %+v", r)
	}
	if m.check {
		t.Errorf("check defaults to false")
	}
}

func TestMove(t *testing.T) {
	m := &fakeManager{}
	h := Handler(t.Context(), m)
	r := decode(t, do(t, h, http.MethodPost, "/api/move", `{"target":"maintenance"}`))
	if !r.OK || m.target != "maintenance" {
		t.Errorf("result = %+v, target %q", r, m.target)
	}
	m.moveErr = manager.ErrBusy
	if rec := do(t, h, http.MethodPost, "/api/move", `{"target":"peak"}`); rec.Code != http.StatusConflict {
		t.Errorf("a move while busy = %d, want 409", rec.Code)
	}
	m.moveErr = fmt.Errorf("%w: can't move while NoCRDs", manager.ErrNotReady)
	if rec := do(t, h, http.MethodPost, "/api/move", `{"target":"peak"}`); rec.Code != http.StatusConflict {
		t.Errorf("a move without the CRDs = %d, want 409", rec.Code)
	}
}

func TestStopMove(t *testing.T) {
	m := &fakeManager{moving: true}
	h := Handler(t.Context(), m)
	if r := decode(t, do(t, h, http.MethodDelete, "/api/move", "")); !r.OK || m.moving {
		t.Errorf("stopping a move = %+v", r)
	}
	if rec := do(t, h, http.MethodDelete, "/api/move", ""); rec.Code != http.StatusConflict {
		t.Errorf("stopping with no move = %d, want 409", rec.Code)
	}
}

func TestInvalidModeWithoutTheCRDs(t *testing.T) {
	m := &fakeManager{setErr: fmt.Errorf("%w: %q isn't one of --known-modes", manager.ErrInvalidMode, "x")}
	rec := do(t, Handler(t.Context(), m), http.MethodPost, "/api/mode", `{"mode":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an invalid mode = %d, want 400", rec.Code)
	}
}

func TestBadRequests(t *testing.T) {
	h := Handler(t.Context(), &fakeManager{})
	if rec := do(t, h, http.MethodPost, "/api/mode", `{"mode":`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rec.Code)
	}
	big := `{"mode":"` + strings.Repeat("a", 5000) + `"}`
	if rec := do(t, h, http.MethodPost, "/api/mode", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body = %d, want 413", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/mode", strings.NewReader(`{"mode":"peak"}`))
	req.Header.Set("Content-Type", "text/plain")
	if rec := send(h, req); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a text/plain body = %d, want 415", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/mode", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on api/mode = %d, want 405", rec.Code)
	}
}

// A page on another site can't change the mode through a tester's
// port-forward; the page itself and programs can.
func TestCrossOriginRequests(t *testing.T) {
	m := &fakeManager{}
	h := Handler(t.Context(), m)
	post := func(header, value string) int {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/mode", strings.NewReader(`{"mode":"peak"}`))
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set(header, value)
		}
		return send(h, req).Code
	}
	if code := post("Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("a cross-site request = %d, want 403", code)
	}
	if code := post("Origin", "https://example.com"); code != http.StatusForbidden {
		t.Errorf("a request from another origin = %d, want 403", code)
	}
	if m.mode != "" {
		t.Errorf("a cross-origin request changed the mode to %q", m.mode)
	}
	if code := post("Sec-Fetch-Site", "same-origin"); code != http.StatusOK {
		t.Errorf("a request from the page = %d, want 200", code)
	}
	if code := post("", ""); code != http.StatusOK {
		t.Errorf("a request from a program = %d, want 200", code)
	}
}

func TestPageAndHeaders(t *testing.T) {
	h := Handler(t.Context(), &fakeManager{})
	for path, want := range map[string]string{"/": "text/html", "/app.js": "text/javascript", "/style.css": "text/css"} {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), want) {
			t.Errorf("%s = %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s has no CSP: %q", path, csp)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s has no nosniff", path)
		}
	}
	if rec := do(t, h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d", rec.Code)
	}
}
