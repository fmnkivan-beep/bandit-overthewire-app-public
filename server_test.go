package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeysAPI(t *testing.T) {
	app := NewApp(newEventHub(), "")
	handler := newHTTPHandler(app, app.events, nil, "127.0.0.1:4173")
	request := func(method, path, host, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:50000", host
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	snapshot := func(want int) {
		rec := request(http.MethodGet, "/api/keys", "127.0.0.1:4173", "")
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("snapshot response invalid")
		}
		var body struct {
			Unlocked []unlockedKey `json:"unlocked"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal("invalid snapshot JSON")
		}
		if body.Unlocked == nil || len(body.Unlocked) != want {
			t.Fatal("unexpected snapshot keys")
		}
		if want == 1 && (body.Unlocked[0].Level != 4 || body.Unlocked[0].Username != "bandit4" || body.Unlocked[0].Password == "") {
			t.Fatal("incorrect snapshot credential")
		}
	}
	snapshot(0)
	app.unlockKey(4)
	snapshot(1)
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/keys"}, {http.MethodPost, "/api/keys/reset"}} {
		for _, guard := range []struct{ host, origin string }{{"example.com", ""}, {"127.0.0.1:4173", "http://example.com"}} {
			if rec := request(route.method, route.path, guard.host, guard.origin); rec.Code != http.StatusForbidden {
				t.Fatal("key API bypassed localOnly")
			}
			if len(app.unlockedKeysSnapshot()) != 1 {
				t.Fatal("rejected request reset keys")
			}
		}
	}
	if rec := request(http.MethodPost, "/api/keys/reset", "127.0.0.1:4173", ""); rec.Code != http.StatusForbidden {
		t.Fatal("key reset accepted missing Origin")
	}
	if rec := request(http.MethodPost, "/api/keys/reset", "127.0.0.1:4173", "http://127.0.0.1:4173"); rec.Code != http.StatusNoContent {
		t.Fatal("reset failed")
	}
	snapshot(0)
}

func TestRequestGuardRejectsNonLocalOrigin(t *testing.T) {
	called := false
	guard := localOnly("127.0.0.1:4173", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, remote, host, origin string
		want                       int
	}{
		{"same-origin", "127.0.0.1:50000", "127.0.0.1:4173", "http://127.0.0.1:4173", http.StatusNoContent},
		{"remote-client", "192.0.2.2:50000", "127.0.0.1:4173", "http://127.0.0.1:4173", http.StatusForbidden},
		{"foreign-origin", "127.0.0.1:50000", "127.0.0.1:4173", "https://example.com", http.StatusForbidden},
		{"wrong-host", "127.0.0.1:50000", "example.com", "http://127.0.0.1:4173", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
			req.RemoteAddr, req.Host = tc.remote, tc.host
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if called != (tc.want == http.StatusNoContent) {
				t.Fatalf("next handler called = %t", called)
			}
		})
	}
}

func TestSSHConnectRejectsInvalidUserBeforeStartingSession(t *testing.T) {
	app := NewApp(newEventHub(), "http://127.0.0.1:4173/internal/askpass")
	handler := newHTTPHandler(app, app.events, nil, "127.0.0.1:4173")
	req := httptest.NewRequest(http.MethodPost, "/api/ssh/connect", bytes.NewBufferString(`{"username":"root","password":"secret"}`))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "127.0.0.1:4173"
	req.Header.Set("Origin", "http://127.0.0.1:4173")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDecodeJSONRejectsOversizedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/ssh/input", strings.NewReader(`{"data":"`+strings.Repeat("x", 32)+`"}`))
	rec := httptest.NewRecorder()
	var payload struct {
		Data string `json:"data"`
	}
	if err := decodeJSON(rec, req, &payload, 16); err == nil {
		t.Fatal("oversized body was accepted")
	}
}

func TestAskpassEndpointConsumesTokenOnce(t *testing.T) {
	app := NewApp(newEventHub(), "http://127.0.0.1:4173/internal/askpass")
	token, cleanup, err := app.startGitAskpass("secret")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	handler := newHTTPHandler(app, app.events, nil, "127.0.0.1:4173")
	for attempt, wantStatus := range []int{http.StatusOK, http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodPost, "/internal/askpass", nil)
		req.RemoteAddr = "127.0.0.1:50000"
		req.Host = "127.0.0.1:4173"
		req.Header.Set(gitAskpassHeader, token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("attempt %d status = %d, want %d", attempt+1, rec.Code, wantStatus)
		}
		if attempt == 0 && rec.Body.String() != "secret" {
			t.Fatalf("password response = %q", rec.Body.String())
		}
	}
}
