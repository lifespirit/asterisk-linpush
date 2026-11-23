package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestParseNumberList(t *testing.T) {
	cases := []struct {
		input string
		count int
		valid bool
	}{
		{"5:10:11", 3, true}, {"5,10,11", 3, true}, {"5", 1, true}, {"5:bad", 0, false}, {"5::10", 0, false},
		{"5;touch /tmp/bad", 0, false}, {"5 10", 0, false}, {"", 0, false}, {"99999999999999999", 0, false},
	}
	for _, tt := range cases {
		n, e := parseNumberList(tt.input)
		if (e == nil) != tt.valid || (tt.valid && len(n) != tt.count) {
			t.Fatalf("%q: %v %+v", tt.input, e, n)
		}
	}
}

// The CLI uses one Unix socket request for a batch, not a loop of individual sends.
func TestWakeCLIBatchUsesOneRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linpush.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var requests atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/wake" || r.Method != "POST" {
			t.Errorf("unexpected HTTP route: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Numbers     []string `json:"numbers"`
			WaitSeconds *int     `json:"wait_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Numbers) != 3 || body.Numbers[0] != "5" || body.Numbers[1] != "10" || body.Numbers[2] != "11" || body.WaitSeconds == nil || *body.WaitSeconds != 0 {
			t.Errorf("unexpected payload: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"sent","numbers":["5","10","11"],"results":[],"sent":1,"failed":0,"invalidated":0,"wait_seconds":0}`)
	})}
	defer server.Close()
	go server.Serve(listener)
	if err := wake([]string{"--numbers", "5:10:11", "--wait", "0", "--socket", path}); err != nil {
		t.Fatal(err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("sent %d CLI requests instead of one", n)
	}
}

func TestWakeModesAreMutuallyExclusive(t *testing.T) {
    cases := [][]string{
        {"--number","5","--wait","0","--wait-register","15"},
        {"--number","5","--wait","0","--wait-ready","15"},
        {"--number","5","--wait-register","15","--wait-ready","15"},
        {"--number","5","--push-grace-ms","300"},
        {"--number","5","--wait-ready","0"},
    }
    for _, args := range cases {
        if err := wake(args); err == nil {
            t.Fatalf("expected wake flags to be rejected: %v",args)
        }
    }
}
