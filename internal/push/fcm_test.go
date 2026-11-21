package push

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFCMHTTPv1(t *testing.T) {
	var authCount atomic.Int32
	var fcmCount atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			authCount.Add(1)
			if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Error("wrong grant type")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		case "/v1/projects/my-proj/messages:send":
			fcmCount.Add(1)
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Error("missing OAuth token")
			}
			b, _ := io.ReadAll(r.Body)
			var req struct {
				Message struct {
					Token   string            `json:"token"`
					Data    map[string]string `json:"data"`
					Android struct {
						Priority string `json:"priority"`
					} `json:"android"`
				} `json:"message"`
			}
			if err := json.Unmarshal(b, &req); err != nil {
				t.Error(err)
			}
			if req.Message.Android.Priority != "HIGH" || req.Message.Data["call-id"] != "test-call" {
				t.Errorf("wrong FCM payload: %s", string(b))
			}
			if req.Message.Token == "dead_token" {
				w.WriteHeader(404)
				io.WriteString(w, `{"error":{"status":"NOT_FOUND","message":"unregistered","details":[{"errorCode":"UNREGISTERED"}]}}`)
				return
			}
			io.WriteString(w, `{"name":"projects/my-proj/messages/1"}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)})
	raw, _ := json.Marshal(accountFile{Type: "service_account", ProjectID: "my-proj", ClientEmail: "service@my-proj.iam.gserviceaccount.com", PrivateKey: string(keyPem), TokenURI: "https://oauth2.googleapis.com/token"})
	c, err := New(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	c.SetEndpoints(ts.URL+"/token", ts.URL)
	c.http = ts.Client()
	if err := c.Send(context.Background(), "good_token", Payload{CallID: "test-call", TTL: 45 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), "good_token", Payload{CallID: "test-call"}); err != nil {
		t.Fatal(err)
	}
	if authCount.Load() != 1 || fcmCount.Load() != 2 {
		t.Fatalf("auth token not cached: auth=%d fcm=%d", authCount.Load(), fcmCount.Load())
	}
	err = c.Send(context.Background(), "dead_token", Payload{CallID: "test-call"})
	if err == nil || !strings.Contains(err.Error(), "UNREGISTERED") {
		t.Fatalf("expected unregistered: %v", err)
	}
}
func mustPKCS8(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	b, e := x509.MarshalPKCS8PrivateKey(k)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
