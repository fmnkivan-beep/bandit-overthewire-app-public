package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newPersistentTestApp(t *testing.T, dir string) *App {
	t.Helper()
	app := NewApp(newEventHub(), "")
	app.workspace = filepath.Join(dir, "Bandit2", "workspace")
	if err := app.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestProgressAPIValidatesOriginSchemaAndPersists(t *testing.T) {
	app := newPersistentTestApp(t, t.TempDir())
	const host = "127.0.0.1:4173"
	handler := newHTTPHandler(app, app.events, nil, host)
	request := func(method, body, origin, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/progress", strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:50000", host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(http.MethodGet, "", "", ""); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET status/cache = %d/%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	valid := `{"version":1,"activeLevelId":"2-3","completed":{"0":true,"2-3":true}}`
	if rec := request(http.MethodPut, valid, "", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("missing Origin status = %d", rec.Code)
	}
	if rec := request(http.MethodPut, valid, "http://"+host, "text/plain"); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong content type status = %d", rec.Code)
	}
	if rec := request(http.MethodPut, `{"version":1,"activeLevelId":"unknown","completed":{}}`, "http://"+host, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid level status = %d", rec.Code)
	}
	if rec := request(http.MethodPut, `{"version":1,"activeLevelId":"2-3","completed":{},"extra":true}`, "http://"+host, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", rec.Code)
	}
	if rec := request(http.MethodPut, `{"version":1,"activeLevelId":"0","activeLevelId":"2-3","completed":{}}`, "http://"+host, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate field status = %d", rec.Code)
	}
	if rec := request(http.MethodPut, `{"Version":1,"activeLevelId":"2-3","completed":{}}`, "http://"+host, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("noncanonical field case status = %d", rec.Code)
	}
	if rec := request(http.MethodPut, `{"version":1,"activeLevelId":"2-3","completed":{"2-3":true}}`, "http://"+host, "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("valid PUT status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := request(http.MethodPut, `{"version":1,"activeLevelId":"`+strings.Repeat("x", progressFileMax)+`","completed":{}}`, "http://"+host, "application/json"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PUT status = %d", rec.Code)
	}
	rec := request(http.MethodGet, "", "", "")
	var saved savedProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil || saved.ActiveLevelID != "2-3" || !saved.Completed["2-3"] {
		t.Fatalf("progress not restored: %#v, err %v", saved, err)
	}
}

func TestProgressResetIsRevisionBoundaryForStaleProgressAndKeys(t *testing.T) {
	dir := t.TempDir()
	current := newPersistentTestApp(t, dir)
	stale := newPersistentTestApp(t, dir)
	key := unlockedKey{Level: 4, Username: "bandit4", Password: "server-confirmed-fixture"}
	current.mu.Lock()
	current.unlockVerifiedKeyLocked(key.Level, key.Password)
	current.mu.Unlock()
	if keys, err := stale.keysSnapshot(); err != nil || len(keys) != 1 {
		t.Fatalf("pre-reset snapshot: count=%d err=%v", len(keys), err)
	}
	reset, err := current.resetProgress()
	if err != nil || reset.Revision != 1 || reset.ActiveLevelID != "0" || reset.Completed["0"] {
		t.Fatalf("reset response state: %#v err=%v", reset, err)
	}
	old := savedProgress{Version: progressVersion, ActiveLevelID: "1-2", Completed: map[string]bool{"0": true}}
	if err := stale.saveProgress(old); err != errStaleProgress {
		t.Fatalf("stale progress save error = %v", err)
	}
	if keys, err := stale.keysSnapshot(); err != nil || len(keys) != 0 {
		t.Fatalf("stale instance restored keys: count=%d err=%v", len(keys), err)
	}
	restarted := newPersistentTestApp(t, dir)
	saved, err := restarted.readProgress()
	if err != nil || saved.Revision != 1 || saved.ActiveLevelID != "0" || saved.Completed["0"] {
		t.Fatalf("restart progress: %#v err=%v", saved, err)
	}
	if keys, err := restarted.keysSnapshot(); err != nil || len(keys) != 0 {
		t.Fatalf("restart archive count=%d err=%v", len(keys), err)
	}
}

func TestProgressResetAPIRequiresExactOriginEmptyJSONAndReturnsCanonicalState(t *testing.T) {
	app := newPersistentTestApp(t, t.TempDir())
	const host = "127.0.0.1:4173"
	handler := newHTTPHandler(app, app.events, nil, host)
	request := func(origin, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/progress/reset", strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:50000", host
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if got := request("http://wrong.test", `{}`); got.Code != http.StatusForbidden {
		t.Fatalf("wrong Origin status = %d", got.Code)
	}
	for _, body := range []string{`{"clear":true}`, `[]`, `{ } {}`} {
		if got := request("http://"+host, body); got.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d", body, got.Code)
		}
	}
	app.mu.Lock()
	app.gitRunning = true
	app.mu.Unlock()
	if got := request("http://"+host, `{}`); got.Code != http.StatusConflict {
		t.Fatalf("busy reset status = %d", got.Code)
	}
	app.mu.Lock()
	app.gitRunning = false
	app.mu.Unlock()
	got := request("http://"+host, `{}`)
	var value savedProgress
	if err := json.Unmarshal(got.Body.Bytes(), &value); got.Code != http.StatusOK || err != nil || value.Revision != 1 || value.ActiveLevelID != "0" || value.Completed["0"] || got.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reset response: status=%d value=%#v err=%v", got.Code, value, err)
	}
}

func TestCorruptProgressIsReportedWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	progressPath := filepath.Join(dir, "Bandit2", "progress.json")
	if err := os.MkdirAll(filepath.Dir(progressPath), 0700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"version":1,"activeLevelId":"invalid"}`)
	if err := os.WriteFile(progressPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	app := newPersistentTestApp(t, dir)
	handler := newHTTPHandler(app, app.events, nil, "127.0.0.1:4173")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := `{"version":1,"activeLevelId":"0","completed":{"0":false}}`
		req := httptest.NewRequest(method, "/api/progress", strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:50000", "127.0.0.1:4173"
		req.Header.Set("Origin", "http://127.0.0.1:4173")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d", method, rec.Code)
		}
	}
	after, err := os.ReadFile(progressPath)
	if err != nil || string(after) != string(corrupt) {
		t.Fatalf("corrupt file changed: %q, err %v", after, err)
	}
}

func TestConfirmedRotatedKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	app := newPersistentTestApp(t, dir)
	rotated := unlockedKey{Level: 7, Username: "bandit7", Password: "server-confirmed-rotated-key"}
	app.mu.Lock()
	app.unlockVerifiedKeyLocked(rotated.Level, rotated.Password)
	app.mu.Unlock()

	restarted := newPersistentTestApp(t, dir)
	keys, err := restarted.keysSnapshot()
	if err != nil || len(keys) != 1 || keys[0] != rotated {
		t.Fatalf("rotated key not restored: %#v, err %v", keys, err)
	}
}

func TestFailedKeyResetKeepsConfirmedKeysAndFile(t *testing.T) {
	dir := t.TempDir()
	app := newPersistentTestApp(t, dir)
	handler := newHTTPHandler(app, app.events, nil, "127.0.0.1:4173")
	key := unlockedKey{Level: 3, Username: "bandit3", Password: "server-confirmed-key"}
	app.mu.Lock()
	app.unlockVerifiedKeyLocked(key.Level, key.Password)
	app.mu.Unlock()
	for _, corrupt := range [][]byte{
		[]byte(`{"version":1}`),
		[]byte(`{"version":1,"unlocked":null}`),
		[]byte(`{"Version":1,"unlocked":[]}`),
		[]byte(`{"version":1,"unlocked":[{"Level":3,"username":"bandit3","password":"server-confirmed-key"}]}`),
		[]byte(`{"version":1,"unlocked":[`),
	} {
		if err := os.WriteFile(app.keysFile, corrupt, 0600); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/keys/reset", nil)
		req.RemoteAddr, req.Host = "127.0.0.1:50000", "127.0.0.1:4173"
		req.Header.Set("Origin", "http://127.0.0.1:4173")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("reset status = %d, want 500", rec.Code)
		}
		if got := app.unlockedKeysSnapshot(); len(got) != 1 || got[0] != key {
			t.Fatalf("failed reset cleared confirmed key: %#v", got)
		}
		after, err := os.ReadFile(app.keysFile)
		if err != nil || string(after) != string(corrupt) {
			t.Fatalf("failed reset changed archive: %q, err %v", after, err)
		}
	}
}

func TestConcurrentInstancesMergeAndRefreshAuthoritativeState(t *testing.T) {
	dir := t.TempDir()
	first := newPersistentTestApp(t, dir)
	second := newPersistentTestApp(t, dir)
	const writes = 16
	completed := make(chan error, writes)
	for i := 0; i < writes; i++ {
		app := first
		if i%2 != 0 {
			app = second
		}
		id := fmt.Sprintf("%d-%d", i, i+1)
		go func(app *App, id string) {
			completed <- app.saveProgress(savedProgress{Version: progressVersion, ActiveLevelID: id, Completed: map[string]bool{id: true}})
		}(app, id)
	}
	for i := 0; i < writes; i++ {
		select {
		case err := <-completed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("storage mutex did not release promptly")
		}
	}
	saved, err := first.readProgress()
	if err != nil || len(saved.Completed) < writes {
		t.Fatalf("concurrent completion flags were lost: %#v, err %v", saved.Completed, err)
	}
	key := unlockedKey{Level: 8, Username: "bandit8", Password: "server-confirmed-rotated-key"}
	first.mu.Lock()
	first.unlockVerifiedKeyLocked(key.Level, key.Password)
	first.mu.Unlock()
	if keys, err := second.keysSnapshot(); err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatalf("second backend snapshot did not load confirmed key: %#v, err %v", keys, err)
	}
	if err := first.resetUnlockedKeys(); err != nil {
		t.Fatal(err)
	}
	if keys, err := second.keysSnapshot(); err != nil || len(keys) != 0 {
		t.Fatalf("next snapshot did not observe reset: %#v, err %v", keys, err)
	}
}
