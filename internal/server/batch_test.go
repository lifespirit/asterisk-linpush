package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.org/asterisk-linphone-push/internal/metrics"
	"example.org/asterisk-linphone-push/internal/push"
	"example.org/asterisk-linphone-push/internal/store"
)

type slowSender struct {
	mu    sync.Mutex
	count int
	delay time.Duration
}

func (s *slowSender) Send(ctx context.Context, token string, p push.Payload) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
	}
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
	return nil
}
func newBatchServer(t *testing.T, sender Sender) *Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{DB: db, FCM: sender, Metrics: metrics.New(), APIToken: "admin-secret-abcdefghijklmnopqrstuvwxyz", MetricsToken: "metrics-secret-abcdefghijklmnopqrstuvwxyz", MaxWait: 30, DefaultWait: 1, TTL: 45}
}
func addBatch(t *testing.T, s *Server, number, token string) {
	t.Helper()
	if err := s.DB.SetEnabled(number, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.UpsertDevice(number, "primary", token, ""); err != nil {
		t.Fatal(err)
	}
}
func TestBatchParallelAndSingleWait(t *testing.T) {
	sender := &slowSender{delay: 180 * time.Millisecond}
	s := newBatchServer(t, sender)
	for i, n := range []string{"5", "10", "11", "12"} {
		addBatch(t, s, n, fmt.Sprintf("device_token_value_%03d_abcdefgh", i))
	}
	one := 1
	start := time.Now()
	out, status, err := s.WakeMany(context.Background(), WakeRequest{Numbers: []string{"5", "10", "11", "12", "5"}, WaitSeconds: &one})
	elapsed := time.Since(start)
	if err != nil || status != 200 || out.Status != "sent" || out.Sent != 4 || out.WaitSeconds != 1 || len(out.Results) != 4 {
		t.Fatalf("unexpected result %+v %d %v", out, status, err)
	}
	if elapsed < time.Second || elapsed > 1550*time.Millisecond {
		t.Fatalf("FCM sends should be concurrent, then one wait; elapsed %v", elapsed)
	}
	sender.mu.Lock()
	sent := sender.count
	sender.mu.Unlock()
	if sent != 4 {
		t.Fatalf("want 4 FCM sends, got %d", sent)
	}
}
func TestBatchTokenDedupAndValidation(t *testing.T) {
	sender := &slowSender{}
	s := newBatchServer(t, sender)
	addBatch(t, s, "5", "same_token_value_1234567890_123456")
	addBatch(t, s, "10", "same_token_value_1234567890_123456")
	zero := 0
	out, _, err := s.WakeMany(context.Background(), WakeRequest{Numbers: []string{"5", "10", "5"}, WaitSeconds: &zero})
	if err != nil || out.Sent != 1 || out.Status != "sent" {
		t.Fatalf("dedup: %+v %v", out, err)
	}
	sender.mu.Lock()
	sent := sender.count
	sender.mu.Unlock()
	if sent != 1 {
		t.Fatalf("duplicate FCM message: %d", sent)
	}
	for _, input := range [][]string{{}, {"5", "not-a-number"}, {"5;rm -rf /"}} {
		if _, code, e := s.WakeMany(context.Background(), WakeRequest{Numbers: input}); e == nil || code != 400 {
			t.Fatalf("invalid list %v accepted", input)
		}
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = "5"
	}
	if _, _, err := s.WakeMany(context.Background(), WakeRequest{Numbers: many}); err == nil {
		t.Fatal("too many input numbers accepted")
	}
}
func TestMetricsAccessAndSeries(t *testing.T) {
	s := newBatchServer(t, &slowSender{})
	addBatch(t, s, "5", "example_token_1234567890123456789")
	zero := 0
	_, _, err := s.WakeMany(context.Background(), WakeRequest{Numbers: []string{"5"}, WaitSeconds: &zero})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, secret string
		want         int
	}{
		{"/metrics", "", 401},
		{"/metrics", s.APIToken, 401},
		{"/metrics", s.MetricsToken, 200},
		{"/v1/extensions", s.MetricsToken, 401},
		{"/v1/extensions", s.APIToken, 200},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.secret != "" {
			r.Header.Set("Authorization", "Bearer "+tc.secret)
		}
		w := httptest.NewRecorder()
		s.PublicHandler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d want %d", tc.path, w.Code, tc.want)
		}
		if tc.path == "/metrics" && tc.want == 200 {
			body := w.Body.String()
			for _, fragment := range []string{
				`linpush_fcm_messages_total{outcome="accepted"} 1`,
				`linpush_wake_requests_total{mode="batch",status="sent"} 1`,
				`linpush_wake_duration_seconds_bucket{le="+Inf"} 1`,
				`linpush_wake_duration_seconds_count 1`,
				`linpush_devices{source="manual",state="active"} 1`,
				`linpush_extensions{push_enabled="true"} 1`,
			} {
				if !strings.Contains(body, fragment) {
					t.Errorf("missing %s", fragment)
				}
			}
			if strings.Contains(body, "example_token") || strings.Contains(body, `number="5"`) {
				t.Error("metrics contain sensitive values")
			}
			if c := w.Header().Get("Content-Type"); !strings.Contains(c, "version=0.0.4") {
				t.Errorf("bad content-type %s", c)
			}
		}
	}
}
func TestInternalBatchEndpoint(t *testing.T) {
	s := newBatchServer(t, &slowSender{})
	addBatch(t, s, "5", "device_token_value_123456789")
	r := httptest.NewRequest("POST", "/wake", strings.NewReader(`{"numbers":["5","11"],"wait_seconds":0}`))
	w := httptest.NewRecorder()
	s.InternalHandler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"sent"`) {
		t.Fatalf("batch internal endpoint: %d %s", w.Code, w.Body.String())
	}
}
