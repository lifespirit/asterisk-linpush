package ami

import (
    "context"
    "strings"
    "sync"
    "time"
)

// RegistrationTracker records fresh FCM SIP registrations, not SIP OPTIONS results.
// A baseline MUST be taken before FCM is sent to avoid losing fast registrations.
type RegistrationTracker struct {
    mu sync.Mutex
    ready bool
    connected bool
    revision map[string]uint64
    contacts map[string]map[string]string
    changed chan struct{}
    refresh chan struct{}
}

func NewRegistrationTracker() *RegistrationTracker {
    return &RegistrationTracker{
        revision: make(map[string]uint64),
        contacts: make(map[string]map[string]string),
        changed: make(chan struct{}),
        refresh: make(chan struct{},1),
    }
}

func (t *RegistrationTracker) notifyLocked(number string) {
    t.revision[number]++
    close(t.changed)
    t.changed = make(chan struct{})
}

// Baseline is called before dispatching FCM. A nil result means AMI is not
// synchronized; callers must use a bounded timeout instead of trusting stale data.
func (t *RegistrationTracker) Baseline(numbers []string) (map[string]uint64, bool) {
    if t == nil { return nil, false }
    t.mu.Lock()
    defer t.mu.Unlock()
    if !t.ready || !t.connected { return nil, false }
    out := make(map[string]uint64, len(numbers))
    for _, n := range numbers { out[n] = t.revision[n] }
    return out, true
}

// OnContactStatus accepts only Updated, which Asterisk sends for REGISTER
// refreshes when send_contact_status_on_update_registration=yes. Reachable and
// Unreachable can be produced by OPTIONS and are deliberately ignored.
func (t *RegistrationTracker) OnContactStatus(m map[string]string) {
    if t == nil || !strings.EqualFold(m["contactstatus"], "Updated") {
        return
    }
    uri := strings.ToLower(m["uri"])
    if !strings.Contains(uri, "pn-provider=fcm") { return }
    aor := m["aor"]
    if !digits.MatchString(aor) { return }
    t.mu.Lock()
    if t.connected { t.notifyLocked(aor) }
    t.mu.Unlock()
}

// ApplySnapshot sees new FCM contacts and refreshed absolute expiration times.
// This also detects registration when AMI did not send the Updated event.
// The first complete snapshot is an initial baseline, never a new registration.
func (t *RegistrationTracker) ApplySnapshot(contacts []map[string]string, projectNumber string) {
    if t == nil { return }
    current := make(map[string]map[string]string)
    for _, m := range contacts {
        c, valid := ParseContact(m, projectNumber)
        if !valid { continue }
        if current[c.Number] == nil { current[c.Number] = make(map[string]string) }
        current[c.Number][c.ID] = m["expirationtime"]
    }
    t.mu.Lock()
    defer t.mu.Unlock()
    if t.ready {
        for number, records := range current {
            previous := t.contacts[number]
            for id, expiration := range records {
                old, exists := previous[id]
                if !exists || (expiration != "" && old != "" && expiration != old) {
                    t.notifyLocked(number)
                    break
                }
            }
        }
    }
    t.contacts = current
    t.ready = true
    t.connected = true
}

// RefreshRequests enables coalesced on-demand AMI snapshots during a wake wait.
func (t *RegistrationTracker) RefreshRequests() <-chan struct{} {
    if t == nil { return nil }
    return t.refresh
}
func (t *RegistrationTracker) requestRefresh() {
    if t == nil { return }
    select { case t.refresh <- struct{}{}: default: }
}

func (t *RegistrationTracker) Disconnected() {
    if t == nil { return }
    t.mu.Lock()
    t.connected = false
    t.mu.Unlock()
}

// Wait blocks until ALL target AoRs have produced a new registration or the
// single shared timeout elapses. The returned map contains confirmed AoRs.
func (t *RegistrationTracker) Wait(ctx context.Context, baseline map[string]uint64, targets []string, timeout time.Duration) map[string]bool {
    done := make(map[string]bool, len(targets))
    if timeout <= 0 || len(targets) == 0 { return done }
    timer := time.NewTimer(timeout)
    defer timer.Stop()
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    if t == nil || baseline == nil {
        select {
        case <-ctx.Done():
        case <-timer.C:
        }
        return done
    }
    for {
        t.mu.Lock()
        complete := true
        for _, n := range targets {
            if t.revision[n] > baseline[n] {
                done[n] = true
            } else {
                complete = false
            }
        }
        changed := t.changed
        t.mu.Unlock()
        if complete { return done }
        select {
        case <-ctx.Done(): return done
        case <-timer.C: return done
        case <-changed:
        case <-ticker.C: t.requestRefresh()
        }
    }
}
