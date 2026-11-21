package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"example.org/asterisk-linphone-push/internal/push"
	"example.org/asterisk-linphone-push/internal/store"
)

type dummy struct {
	mu    sync.Mutex
	sends []push.Payload
	bad   string
}

func (d *dummy) Send(_ context.Context, token string, p push.Payload) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sends = append(d.sends, p)
	if token == d.bad {
		return &push.SendError{Code: "UNREGISTERED", Message: "token invalid", Permanent: true}
	}
	return nil
}
func TestAPIAndWake(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock := &dummy{bad: "bad_token_aaaaaaaaaaaaaaaaaaaa"}
	srv := &Server{DB: db, FCM: mock, APIToken: "valid_token_abcdefghijklmnopqrst12345", DefaultWait: 1, MaxWait: 30, TTL: 45, ProjectNumber: "12345"}
	api := srv.PublicHandler()
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		var payload []byte
		if body != "" {
			payload = []byte(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}
	if x := request("GET", "/v1/extensions", "", ""); x.Code != 401 {
		t.Fatalf("unauthenticated: %d", x.Code)
	}
	if x := request("PUT", "/v1/extensions/101", `{"push_enabled":true}`, srv.APIToken); x.Code != 200 {
		t.Fatalf("set extension: %s", x.Body.String())
	}
	if x := request("PUT", "/v1/extensions/101/devices/a", `{"token":"good_token_aaaaaaaaaaaaaaaaaaaa","pn_param":"12345"}`, srv.APIToken); x.Code != 200 {
		t.Fatalf("put device: %s", x.Body.String())
	}
	if x := request("PUT", "/v1/extensions/101/devices/b", `{"token":"bad_token_aaaaaaaaaaaaaaaaaaaa","pn_param":"12345"}`, srv.APIToken); x.Code != 200 {
		t.Fatalf("put device 2: %s", x.Body.String())
	}
	if x := request("GET", "/v1/extensions/101/devices", "", srv.APIToken); bytes.Contains(x.Body.Bytes(), []byte("good_token_")) {
		t.Fatalf("token exposed: %s", x.Body.String())
	}
	zero := 0
	res, status, err := srv.Wake(context.Background(), WakeRequest{Number: "101", WaitSeconds: &zero, CallID: "call123"})
	if err != nil || status != 200 || res.Sent != 1 || res.Failed != 1 || res.Invalidated != 1 {
		t.Fatalf("wake: %+v %d %v", res, status, err)
	}
	if mock.sends[0].CallID != "call123" {
		t.Fatalf("metadata missing: %+v", mock.sends[0])
	}
	x := request("PUT", "/v1/extensions/101", `{"push_enabled":false}`, srv.APIToken)
	if x.Code != 200 {
		t.Fatal(x.Body)
	}
	before := time.Now()
	res, _, err = srv.Wake(context.Background(), WakeRequest{Number: "101"})
	if err != nil || res.Status != "disabled" || time.Since(before) > 300*time.Millisecond {
		t.Fatalf("disabled must be immediate: %+v %v", res, err)
	}
	if x := request("PUT", "/v1/extensions/101/devices/c", `{"token":"cccccccccccccccccccccccc","pn_param":"999"}`, srv.APIToken); x.Code != 400 {
		t.Fatal("pn_param mismatch allowed")
	}
	if x := request("GET", "/v1/extensions", "", srv.APIToken); x.Code != 200 {
		t.Fatal(x.Body)
	} else {
		var ex []store.Extension
		if err := json.Unmarshal(x.Body.Bytes(), &ex); err != nil || len(ex) != 1 {
			t.Fatal(x.Body.String())
		}
	}
	if x := request("DELETE", "/v1/extensions/101/devices/a", "", srv.APIToken); x.Code != 204 {
		t.Fatal(x.Body)
	}
}
func TestInternalWakeAndWait(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.SetEnabled("102", true)
	_ = db.UpsertDevice("102", "android", "good_token_aaaaaaaaaaaaaaaaaaaa", "")
	srv := &Server{DB: db, FCM: &dummy{}, DefaultWait: 1, MaxWait: 10, TTL: 45}
	r := httptest.NewRequest("POST", "http://unix/wake", bytes.NewBufferString(`{"number":"102"}`))
	w := httptest.NewRecorder()
	start := time.Now()
	srv.InternalHandler().ServeHTTP(w, r)
	if w.Code != 200 || time.Since(start) < time.Second {
		t.Fatalf("server must wait: status=%d elapsed=%v", w.Code, time.Since(start))
	}
}

func TestAutoFirstThenManualFallback(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.SetEnabled("5", true)
	_ = db.UpsertDevice("5", "emergency", "good_manual_token_12345678901", "")
	_ = db.ApplyAMISnapshot([]store.AutoContact{{Number: "5", ID: "auto-new", Token: "bad_automatic_token_12345678901", PNParam: "123"}}, nil)
	zero := 0
	fcm := &dummy{bad: "bad_automatic_token_12345678901"}
	srv := &Server{DB: db, FCM: fcm, DefaultWait: 0, MaxWait: 5, TTL: 45}
	result, code, err := srv.Wake(context.Background(), WakeRequest{Number: "5", WaitSeconds: &zero})
	if err != nil || code != 200 || result.Sent != 1 || result.Failed != 1 || result.Invalidated != 1 {
		t.Fatalf("manual emergency fallback did not work: %+v %d %v", result, code, err)
	}
	if len(fcm.sends) != 2 {
		t.Fatalf("expected two sends: %+v", fcm.sends)
	}
	fcm = &dummy{}
	srv.FCM = fcm
	_ = db.ApplyAMISnapshot([]store.AutoContact{{Number: "5", ID: "auto-newer", Token: "good_auto_token_12345678901", PNParam: "123"}}, map[string]bool{"5": true})
	result, code, err = srv.Wake(context.Background(), WakeRequest{Number: "5", WaitSeconds: &zero})
	if err != nil || code != 200 || result.Sent != 1 || len(fcm.sends) != 1 || fcm.sends[0].DeviceID != "auto-newer" {
		t.Fatalf("manual should not be used on automatic success: %+v %d %v %+v", result, code, err, fcm.sends)
	}
}
