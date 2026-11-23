package server

import (
    "net/http"
    "strings"
)

// /control is only registered on the Unix socket owned by linpush:asterisk.
// The dialplan MUST derive the target number from an authenticated endpoint.
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
    number := strings.TrimPrefix(r.URL.Path, "/control/")
    if !validNumber(number) {
        fail(w, 400, "invalid extension number")
        return
    }
    if r.Method == http.MethodPost {
        var req struct { Enabled *bool `json:"enabled"` }
        if err := decode(r, &req); err != nil || req.Enabled == nil {
            fail(w, 400, "expected enabled boolean")
            return
        }
        if err := s.DB.SetEnabled(number, *req.Enabled); err != nil {
            if s.Metrics != nil { s.Metrics.IVR("enable", "error") }
            fail(w, 500, "database update failed")
            return
        }
        if s.Metrics != nil {
            if *req.Enabled { s.Metrics.IVR("enable", "success") } else { s.Metrics.IVR("disable", "success") }
        }
    } else if r.Method != http.MethodGet {
        fail(w, 405, "method not allowed")
        return
    }
    ext, found, err := s.DB.GetExtension(number)
    if err != nil {
        if r.Method == http.MethodGet && s.Metrics != nil { s.Metrics.IVR("status", "error") }
        fail(w, 500, "database query failed")
        return
    }
    if r.Method == http.MethodGet && s.Metrics != nil { s.Metrics.IVR("status", "success") }
    if !found || !ext.PushEnabled {
        jsonOut(w, 200, map[string]string{"status":"disabled"})
    } else {
        jsonOut(w, 200, map[string]string{"status":"enabled"})
    }
}
