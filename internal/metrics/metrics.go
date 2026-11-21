// Package metrics provides a low-cardinality Prometheus 0.0.4 text exporter.
// It intentionally uses only the Go standard library.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

var durationBounds = []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 90}
var targetBounds = []float64{1, 2, 3, 5, 10, 20, 32}

type hist struct {
	counts []uint64
	sum    float64
	n      uint64
}

func newHist(bounds []float64) hist { return hist{counts: make([]uint64, len(bounds))} }
func (h *hist) observe(v float64, bounds []float64) {
	h.n++
	h.sum += v
	for i, b := range bounds {
		if v <= b {
			h.counts[i]++
		}
	}
}

type Registry struct {
	mu           sync.Mutex
	started      time.Time
	active       int64
	wake         map[string]uint64
	fcm          map[string]uint64
	duration     hist
	wait         hist
	targets      hist
	amiConnected bool
	amiSuccess   float64
	amiContacts  int
	amiTokens    int
	amiErrors    uint64
	amiSuccesses uint64
}

func New() *Registry {
	return &Registry{started: time.Now(), wake: make(map[string]uint64), fcm: make(map[string]uint64), duration: newHist(durationBounds), wait: newHist(durationBounds), targets: newHist(targetBounds)}
}
func (r *Registry) Start() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.active++
	r.mu.Unlock()
}
func (r *Registry) Wake(mode, status string, duration, wait time.Duration, targets int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	if r.active < 0 {
		r.active = 0
	}
	r.wake[mode+":"+status]++
	r.duration.observe(duration.Seconds(), durationBounds)
	r.wait.observe(wait.Seconds(), durationBounds)
	r.targets.observe(float64(targets), targetBounds)
}
func (r *Registry) FCM(outcome string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.fcm[outcome]++
	r.mu.Unlock()
}
func (r *Registry) AMI(connected bool, success bool, failed bool, contacts, tokens int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.amiConnected = connected
	if success {
		r.amiSuccesses++
		r.amiSuccess = float64(time.Now().Unix())
		r.amiContacts = contacts
		r.amiTokens = tokens
	}
	if failed {
		r.amiErrors++
	}
}
func emitHist(w io.Writer, name, help string, h hist, bounds []float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, b := range bounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, b, h.counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, h.n, name, h.sum, name, h.n)
}
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprint(w, "# HELP linpush_up Service process is running.\n# TYPE linpush_up gauge\nlinpush_up 1\n")
	fmt.Fprintf(w, "# HELP linpush_start_time_seconds Process start Unix timestamp.\n# TYPE linpush_start_time_seconds gauge\nlinpush_start_time_seconds %d\n", r.started.Unix())
	fmt.Fprintf(w, "# HELP linpush_wake_inflight Current active wake requests.\n# TYPE linpush_wake_inflight gauge\nlinpush_wake_inflight %d\n", r.active)
	fmt.Fprint(w, "# HELP linpush_wake_requests_total Wake requests by mode and outcome.\n# TYPE linpush_wake_requests_total counter\n")
	for _, mode := range []string{"single", "batch"} {
		for _, status := range []string{"sent", "disabled", "no_devices", "send_failed", "deduplicated", "skipped", "error"} {
			fmt.Fprintf(w, "linpush_wake_requests_total{mode=\"%s\",status=\"%s\"} %d\n", mode, status, r.wake[mode+":"+status])
		}
	}
	fmt.Fprint(w, "# HELP linpush_fcm_messages_total Firebase send attempts by outcome.\n# TYPE linpush_fcm_messages_total counter\n")
	for _, status := range []string{"accepted", "failed", "unregistered"} {
		fmt.Fprintf(w, "linpush_fcm_messages_total{outcome=\"%s\"} %d\n", status, r.fcm[status])
	}
	emitHist(w, "linpush_wake_duration_seconds", "Time from wake request start to completion.", r.duration, durationBounds)
	emitHist(w, "linpush_wake_wait_seconds", "Actual requested post-FCM wait duration.", r.wait, durationBounds)
	emitHist(w, "linpush_wake_targets", "Number of distinct target extensions in a request.", r.targets, targetBounds)
	conn := 0
	if r.amiConnected {
		conn = 1
	}
	fmt.Fprintf(w, "# HELP linpush_ami_connected AMI connection state (1 for connected).\n# TYPE linpush_ami_connected gauge\nlinpush_ami_connected %d\n", conn)
	fmt.Fprintf(w, "# HELP linpush_ami_last_success_timestamp_seconds Timestamp of last complete AMI contact snapshot.\n# TYPE linpush_ami_last_success_timestamp_seconds gauge\nlinpush_ami_last_success_timestamp_seconds %g\n", r.amiSuccess)
	fmt.Fprintf(w, "# HELP linpush_ami_contacts Number of contacts on last complete AMI snapshot.\n# TYPE linpush_ami_contacts gauge\nlinpush_ami_contacts %d\n", r.amiContacts)
	fmt.Fprintf(w, "# HELP linpush_ami_fcm_contacts Number of FCM contacts on last complete AMI snapshot.\n# TYPE linpush_ami_fcm_contacts gauge\nlinpush_ami_fcm_contacts %d\n", r.amiTokens)
	fmt.Fprintf(w, "# HELP linpush_ami_sync_total AMI snapshot outcomes.\n# TYPE linpush_ami_sync_total counter\nlinpush_ami_sync_total{result=\"success\"} %d\nlinpush_ami_sync_total{result=\"error\"} %d\n", r.amiSuccesses, r.amiErrors)
}
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}
