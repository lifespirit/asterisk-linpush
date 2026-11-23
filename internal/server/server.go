package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"example.org/asterisk-linphone-push/internal/ami"
    "example.org/asterisk-linphone-push/internal/metrics"
	"example.org/asterisk-linphone-push/internal/push"
	"example.org/asterisk-linphone-push/internal/store"
)

var extPattern = regexp.MustCompile(`^[0-9]{1,16}$`)
var devicePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type Sender interface {
	Send(context.Context, string, push.Payload) error
}

const MaxBatchTargets = 32

type Server struct {
	Metrics       *metrics.Registry
	limitOnce     sync.Once
	sendSlots     chan struct{}
	DB            *store.Store
	FCM           Sender
	APIToken      string
	MetricsToken  string
	ProjectNumber string
	DefaultWait   int
	MaxWait       int
	TTL           int
	SyncStatus    func() any
    Registration *ami.RegistrationTracker
}
type WakeRequest struct {
	Number      string   `json:"number,omitempty"`
	Numbers     []string `json:"numbers,omitempty"`
	WaitSeconds *int     `json:"wait_seconds,omitempty"`
    WaitRegisterSeconds *int `json:"wait_register_seconds,omitempty"`
	CallID      string   `json:"call_id,omitempty"`
	FromURI     string   `json:"from_uri,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
}
type WakeResult struct {
	Status      string `json:"status"`
	Number      string `json:"number"`
	Sent        int    `json:"sent"`
	Failed      int    `json:"failed"`
	Invalidated int    `json:"invalidated"`
	WaitSeconds int    `json:"wait_seconds"`
    Registration string `json:"registration,omitempty"`
}

type BatchWakeResult struct {
	Status      string       `json:"status"`
	Numbers     []string     `json:"numbers"`
	Results     []WakeResult `json:"results"`
	Sent        int          `json:"sent"`
	Failed      int          `json:"failed"`
	Invalidated int          `json:"invalidated"`
	WaitSeconds int          `json:"wait_seconds"`
    Registration string `json:"registration,omitempty"`
}

func (s *Server) slot() chan struct{} {
	s.limitOnce.Do(func() { s.sendSlots = make(chan struct{}, 8) })
	return s.sendSlots
}
func (s *Server) observe(mode, status string, start time.Time, wait time.Duration, targets int) {
	if s.Metrics != nil {
		s.Metrics.Wake(mode, status, time.Since(start), wait, targets)
	}
}
func parseTargets(req WakeRequest) ([]string, error) {
	if req.Number != "" && len(req.Numbers) > 0 {
		return nil, errors.New("use either number or numbers, not both")
	}
	if req.Number != "" {
		if !validNumber(req.Number) {
			return nil, errors.New("invalid extension number")
		}
		return []string{req.Number}, nil
	}
	if len(req.Numbers) == 0 {
		return nil, errors.New("missing numbers")
	}
	if len(req.Numbers) > MaxBatchTargets {
		return nil, fmt.Errorf("too many numbers (max %d)", MaxBatchTargets)
	}
	used := make(map[string]bool, len(req.Numbers))
	targets := make([]string, 0, len(req.Numbers))
	for _, n := range req.Numbers {
		if !validNumber(n) {
			return nil, errors.New("invalid extension number")
		}
		if !used[n] {
			used[n] = true
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("missing numbers")
	}
	sort.Strings(targets)
	return targets, nil
}
func validNumber(s string) bool { return extPattern.MatchString(s) }
func jsonOut(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, msg string) {
	jsonOut(w, status, map[string]string{"error": msg})
}
func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("missing JSON body")
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 16*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var more any
	if err := d.Decode(&more); err != io.EOF {
		return errors.New("extra data after JSON")
	}
	return nil
}
func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || s.APIToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(s.APIToken)) == 1
}
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		counts, err := s.DB.MetricsCounts()
		if err != nil {
			fail(w, 503, "database metrics unavailable")
			return
		}
		var b bytes.Buffer
		if s.Metrics != nil {
			s.Metrics.Write(&b)
		} else {
			metrics.New().Write(&b)
		}
		fmt.Fprintf(&b, "# HELP linpush_devices Number of stored push devices by source and activation state.\n# TYPE linpush_devices gauge\n")
		fmt.Fprintf(&b, "linpush_devices{source=\"asterisk\",state=\"active\"} %d\n", counts.AutoActive)
		fmt.Fprintf(&b, "linpush_devices{source=\"asterisk\",state=\"inactive\"} %d\n", counts.AutoInactive)
		fmt.Fprintf(&b, "linpush_devices{source=\"manual\",state=\"active\"} %d\n", counts.ManualActive)
		fmt.Fprintf(&b, "linpush_devices{source=\"manual\",state=\"inactive\"} %d\n", counts.ManualInactive)
		fmt.Fprintf(&b, "# HELP linpush_extensions Number of stored extensions by push permission.\n# TYPE linpush_extensions gauge\n")
		fmt.Fprintf(&b, "linpush_extensions{push_enabled=\"true\"} %d\n", counts.EnabledExtensions)
		fmt.Fprintf(&b, "linpush_extensions{push_enabled=\"false\"} %d\n", counts.DisabledExtensions)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write(b.Bytes())
	})
	mux.HandleFunc("POST /v1/wake", func(w http.ResponseWriter, r *http.Request) {
		var req WakeRequest
		if err := decode(r, &req); err != nil {
			fail(w, 400, "invalid JSON: "+err.Error())
			return
		}
		result, status, err := s.WakeMany(r.Context(), req)
		if err != nil {
			fail(w, status, err.Error())
			return
		}
		jsonOut(w, 200, result)
	})
	mux.HandleFunc("GET /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		if s.SyncStatus == nil {
			fail(w, 404, "AMI synchronization is disabled")
			return
		}
		jsonOut(w, 200, s.SyncStatus())
	})
	mux.HandleFunc("/v1/extensions", s.handleExtensions)
	mux.HandleFunc("/v1/extensions/", s.handleExtensions)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated := s.authorized(r)
		if r.URL.Path == "/metrics" && s.MetricsToken != "" {
			header := r.Header.Get("Authorization")
			if strings.HasPrefix(header, "Bearer ") {
				authenticated = subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(s.MetricsToken)) == 1
			} else {
				authenticated = false
			}
		}
		if r.URL.Path != "/healthz" && !authenticated {
			w.Header().Set("WWW-Authenticate", `Bearer realm="linpush"`)
			fail(w, 401, "unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /wake", func(w http.ResponseWriter, r *http.Request) {
		var req WakeRequest
		if err := decode(r, &req); err != nil {
			fail(w, 400, "invalid JSON: "+err.Error())
			return
		}
		if len(req.Numbers) > 0 {
			result, status, err := s.WakeMany(r.Context(), req)
			if err != nil {
				fail(w, status, err.Error())
				return
			}
			jsonOut(w, 200, result)
			return
		}
		result, status, err := s.Wake(r.Context(), req)
		if err != nil {
			fail(w, status, err.Error())
			return
		}
		jsonOut(w, 200, result)
	})
	mux.HandleFunc("/control/", s.handleControl)
    return mux
}
func (s *Server) handleExtensions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/extensions" {
		if r.Method != http.MethodGet {
			fail(w, 405, "method not allowed")
			return
		}
		v, err := s.DB.ListExtensions()
		if err != nil {
			fail(w, 500, "database query failed")
			return
		}
		jsonOut(w, 200, v)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/extensions/")
	parts := strings.Split(rest, "/")
	number := parts[0]
	if !validNumber(number) {
		fail(w, 400, "extension must contain only 1-16 digits")
		return
	}
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			value, ok, err := s.DB.GetExtension(number)
			if err != nil {
				fail(w, 500, "database query failed")
				return
			}
			if !ok {
				fail(w, 404, "extension not found")
				return
			}
			jsonOut(w, 200, value)
		case http.MethodPut:
			var body struct {
				PushEnabled *bool `json:"push_enabled"`
			}
			if err := decode(r, &body); err != nil || body.PushEnabled == nil {
				fail(w, 400, "expected JSON with push_enabled boolean")
				return
			}
			if err := s.DB.SetEnabled(number, *body.PushEnabled); err != nil {
				fail(w, 500, "database update failed")
				return
			}
			val, _, _ := s.DB.GetExtension(number)
			jsonOut(w, 200, val)
		default:
			fail(w, 405, "method not allowed")
		}
	case len(parts) == 2 && parts[1] == "devices":
		if r.Method != http.MethodGet {
			fail(w, 405, "method not allowed")
			return
		}
		ds, err := s.DB.ListDevices(number, false)
		if err != nil {
			fail(w, 500, "database query failed")
			return
		}
		jsonOut(w, 200, ds)
	case len(parts) == 3 && parts[1] == "devices":
		id := parts[2]
		if !devicePattern.MatchString(id) || strings.HasPrefix(id, "auto-") {
			fail(w, 400, "invalid device_id")
			return
		}
		switch r.Method {
		case http.MethodPut:
			var body struct {
				Token   string `json:"token"`
				PNParam string `json:"pn_param"`
			}
			if err := decode(r, &body); err != nil {
				fail(w, 400, "invalid JSON")
				return
			}
			if len(body.Token) < 20 || len(body.Token) > 4096 || strings.ContainsAny(body.Token, " \n\r\t\x00\"'\\") {
				fail(w, 400, "invalid FCM token")
				return
			}
			if body.PNParam != "" {
				if !validNumber(body.PNParam) || s.ProjectNumber != "" && body.PNParam != s.ProjectNumber {
					fail(w, 400, "pn_param must match Firebase project number")
					return
				}
			}
			if err := s.DB.UpsertDevice(number, id, body.Token, body.PNParam); err != nil {
				fail(w, 500, "database update failed")
				return
			}
			jsonOut(w, 200, map[string]string{"status": "saved", "number": number, "device_id": id})
		case http.MethodDelete:
			if err := s.DB.RemoveDevice(number, id); err != nil {
				fail(w, 500, "database delete failed")
				return
			}
			w.WriteHeader(204)
		default:
			fail(w, 405, "method not allowed")
		}
	case len(parts) == 2 && parts[1] == "wake":
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
			return
		}
		var req WakeRequest
		if err := decode(r, &req); err != nil {
			fail(w, 400, "invalid JSON: "+err.Error())
			return
		}
		req.Number = number
		result, status, err := s.Wake(r.Context(), req)
		if err != nil {
			fail(w, status, err.Error())
			return
		}
		jsonOut(w, 200, result)
	default:
		fail(w, 404, "not found")
	}
}

// Wake preserves the v0.2.0 single-target API and semantics.
func (s *Server) Wake(ctx context.Context, req WakeRequest) (out WakeResult, status int, err error) {
	start := time.Now()
	if s.Metrics != nil {
		s.Metrics.Start()
	}
	defer func() {
		label := "error"
		wait := time.Duration(0)
		if err == nil {
			label = out.Status
			wait = time.Duration(out.WaitSeconds) * time.Second
		}
		s.observe("single", label, start, wait, 1)
	}()
	if req.WaitRegisterSeconds == nil { return s.wakeOne(ctx, req, nil) }
    if *req.WaitRegisterSeconds < 0 || *req.WaitRegisterSeconds > s.MaxWait {
        return WakeResult{}, 400, errors.New("invalid wait_register_seconds")
    }
    if req.WaitSeconds != nil && *req.WaitSeconds != 0 {
        return WakeResult{}, 400, errors.New("wait_seconds and wait_register_seconds cannot be combined")
    }
    baseline := s.registrationBaseline([]string{req.Number})
    zero := 0
    req.WaitSeconds = &zero
    out, status, err = s.wakeOne(ctx, req, nil)
    if err != nil { return out, status, err }
    if out.Sent > 0 {
        ready, elapsed := s.registrationWait(ctx, baseline, []string{req.Number}, *req.WaitRegisterSeconds)
        out.WaitSeconds = elapsed
        if ready[req.Number] { out.Registration = "registered" } else { out.Registration = "timeout" }
    }
    return out, status, nil
}

// WakeMany dispatches targets concurrently, waits once after all FCM sends.
// Its shared map deduplicates the same FCM registration token across extensions.
func (s *Server) WakeMany(ctx context.Context, req WakeRequest) (out BatchWakeResult, status int, err error) {
	start := time.Now()
	if s.Metrics != nil {
		s.Metrics.Start()
	}
	targetCount := 0
	defer func() {
		label := "error"
		wait := time.Duration(0)
		if err == nil {
			label = out.Status
			wait = time.Duration(out.WaitSeconds) * time.Second
		}
		s.observe("batch", label, start, wait, targetCount)
	}()
	targets, parseErr := parseTargets(req)
	if parseErr != nil {
		return BatchWakeResult{}, 400, parseErr
	}
	targetCount = len(targets)
    var baseline map[string]uint64
    if req.WaitRegisterSeconds != nil {
        if *req.WaitRegisterSeconds < 0 || *req.WaitRegisterSeconds > s.MaxWait {
            return BatchWakeResult{}, 400, errors.New("invalid wait_register_seconds")
        }
        if req.WaitSeconds != nil && *req.WaitSeconds != 0 {
            return BatchWakeResult{}, 400, errors.New("wait_seconds and wait_register_seconds cannot be combined")
        }
        baseline = s.registrationBaseline(targets)
    }
	if len(req.CallID) > 150 || len(req.FromURI) > 256 || len(req.DisplayName) > 128 || strings.ContainsAny(req.CallID+req.FromURI+req.DisplayName, "\r\n\x00") {
		return BatchWakeResult{}, 400, errors.New("invalid push metadata")
	}
	wait := s.DefaultWait
	if req.WaitSeconds != nil {
		wait = *req.WaitSeconds
	}
    if req.WaitRegisterSeconds != nil { wait = 0 }
	if wait < 0 || wait > s.MaxWait {
		return BatchWakeResult{}, 400, fmt.Errorf("wait_seconds must be 0..%d", s.MaxWait)
	}
	out = BatchWakeResult{Numbers: targets, Results: make([]WakeResult, len(targets)), Status: "skipped"}
	zero := 0
	var wg sync.WaitGroup
	var dedup sync.Map
	var firstErr error
	var errMu sync.Mutex
	for i, n := range targets {
		wg.Add(1)
		go func(index int, number string) {
			defer wg.Done()
			one := req
			one.Number = number
			one.Numbers = nil
            one.WaitRegisterSeconds = nil
			one.WaitSeconds = &zero
			res, code, e := s.wakeOne(ctx, one, &dedup)
			if e != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("wake %s: %w", number, e)
					status = code
				}
				errMu.Unlock()
				return
			}
			out.Results[index] = res
		}(i, n)
	}
	wg.Wait()
	if firstErr != nil {
		return BatchWakeResult{}, status, firstErr
	}
	var disabled, noDevices, deduplicated int
	for _, v := range out.Results {
		out.Sent += v.Sent
		out.Failed += v.Failed
		out.Invalidated += v.Invalidated
		switch v.Status {
		case "disabled":
			disabled++
		case "no_devices":
			noDevices++
		case "deduplicated":
			deduplicated++
		}
	}
	switch {
	case out.Sent > 0:
		out.Status = "sent"
	case out.Failed > 0:
		out.Status = "send_failed"
	case disabled == len(targets):
		out.Status = "disabled"
	case noDevices+disabled == len(targets):
		out.Status = "no_devices"
	case deduplicated > 0:
		out.Status = "deduplicated"
	}
	if out.Sent > 0 && req.WaitRegisterSeconds != nil {
        waiting := make([]string, 0, len(targets))
        for _, res := range out.Results { if res.Sent > 0 { waiting = append(waiting, res.Number) } }
        ready, elapsed := s.registrationWait(ctx, baseline, waiting, *req.WaitRegisterSeconds)
        out.WaitSeconds = elapsed
        all := true
        for i := range out.Results {
            if out.Results[i].Sent > 0 {
                if ready[out.Results[i].Number] {
                    out.Results[i].Registration = "registered"
                } else {
                    out.Results[i].Registration = "timeout"
                    all = false
                }
            }
        }
        if all { out.Registration = "registered" } else { out.Registration = "timeout" }
    } else if out.Sent > 0 && wait > 0 {
		select {
		case <-ctx.Done():
			return BatchWakeResult{}, 499, ctx.Err()
		case <-time.After(time.Duration(wait) * time.Second):
		}
		out.WaitSeconds = wait
	}
	return out, 200, nil
}

func (s *Server) wakeOne(ctx context.Context, req WakeRequest, dedup *sync.Map) (WakeResult, int, error) {
	if !validNumber(req.Number) {
		return WakeResult{}, 400, errors.New("invalid extension number")
	}
	if len(req.CallID) > 150 || len(req.FromURI) > 256 || len(req.DisplayName) > 128 || strings.ContainsAny(req.CallID+req.FromURI+req.DisplayName, "\r\n\x00") {
		return WakeResult{}, 400, errors.New("invalid push metadata")
	}
	wait := s.DefaultWait
	if req.WaitSeconds != nil {
		wait = *req.WaitSeconds
	}
	if wait < 0 || wait > s.MaxWait {
		return WakeResult{}, 400, fmt.Errorf("wait_seconds must be 0..%d", s.MaxWait)
	}
	out := WakeResult{Number: req.Number, Status: "skipped"}
	ext, exists, err := s.DB.GetExtension(req.Number)
	if err != nil {
		return WakeResult{}, 500, errors.New("database query failed")
	}
	if !exists || !ext.PushEnabled {
		out.Status = "disabled"
		return out, 200, nil
	}
	devices, err := s.DB.ListDevices(req.Number, true)
	if err != nil {
		return WakeResult{}, 500, errors.New("database query failed")
	}
	if len(devices) == 0 {
		out.Status = "no_devices"
		return out, 200, nil
	}
	// Prefer tokens obtained directly from verified SIP registrations.
	// Manual tokens are emergency fallback only, and never get deleted by AMI.
	automatic := make([]store.Device, 0)
	manual := make([]store.Device, 0)
	for _, d := range devices {
		if d.Source == "asterisk" {
			automatic = append(automatic, d)
		} else {
			manual = append(manual, d)
		}
	}
	duplicates := 0
	sendBatch := func(batch []store.Device) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, dev := range batch {
			wg.Add(1)
			go func(d store.Device) {
				defer wg.Done()
				if dedup != nil {
					if _, found := dedup.LoadOrStore(d.Token, struct{}{}); found {
						mu.Lock()
						duplicates++
						mu.Unlock()
						return
					}
				}
				select {
				case s.slot() <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-s.slot() }()
				child, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				err := s.FCM.Send(child, d.Token, push.Payload{CallID: req.CallID, FromURI: req.FromURI, DisplayName: req.DisplayName, DeviceID: d.ID, TTL: time.Duration(s.TTL) * time.Second})
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					if s.Metrics != nil {
						s.Metrics.FCM("accepted")
					}
					out.Sent++
					return
				}
				out.Failed++
				var pe *push.SendError
				if s.Metrics != nil {
					if errors.As(err, &pe) && pe.Permanent {
						s.Metrics.FCM("unregistered")
					} else {
						s.Metrics.FCM("failed")
					}
				}
				if errors.As(err, &pe) && pe.Permanent {
					if e := s.DB.DisableDevice(req.Number, d.ID, pe.Code); e == nil {
						out.Invalidated++
					}
				}
				// FCM errors can expose request details; never print full tokens.
				if errors.As(err, &pe) {
					log.Printf("FCM send failed ext=%s device=%s code=%s", req.Number, d.ID, pe.Code)
				} else {
					log.Printf("FCM send failed ext=%s device=%s", req.Number, d.ID)
				}
			}(dev)
		}
		wg.Wait()
	}
	if len(automatic) > 0 {
		sendBatch(automatic)
	}
	if out.Sent == 0 && len(manual) > 0 {
		sendBatch(manual)
	}

	if out.Sent == 0 && out.Failed == 0 && duplicates > 0 {
		out.Status = "deduplicated"
		return out, 200, nil
	}
	if out.Sent == 0 {
		out.Status = "send_failed"
		return out, 200, nil
	}
	out.Status = "sent"
	out.WaitSeconds = wait
	if wait > 0 {
		select {
		case <-ctx.Done():
			return WakeResult{}, 499, ctx.Err()
		case <-time.After(time.Duration(wait) * time.Second):
		}
	}
	return out, 200, nil
}
