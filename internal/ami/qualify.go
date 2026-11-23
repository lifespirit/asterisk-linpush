package ami

import (
    "crypto/rand"
    "encoding/hex"
    "errors"
    "strings"
)

type QualifyRequest struct {
    ActionID string
    Number string
}

type qualifyWaiter struct {
    Number string
    Channel chan bool
}

// Probe requests a fresh Asterisk PJSIPQualify transaction. Success is signaled
// only by a correlated LinpushQualifyResult emitted by the optional Asterisk patch.
// Unpatched Asterisk cannot falsely satisfy the result.
func (t *RegistrationTracker) Probe(number string) (<-chan bool, func(), error) {
    if t == nil || !digits.MatchString(number) { return nil, nil, errors.New("invalid qualify target") }
    idBytes := make([]byte, 16)
    if _, err := rand.Read(idBytes); err != nil { return nil,nil,err }
    id := "linpush-" + hex.EncodeToString(idBytes)
    ch := make(chan bool, 1)
    t.mu.Lock()
    if !t.connected || !t.ready {
        t.mu.Unlock()
        return nil,nil,errors.New("AMI disconnected")
    }
    if t.qualifyWaiters == nil { t.qualifyWaiters=make(map[string]qualifyWaiter) }
    t.qualifyWaiters[id] = qualifyWaiter{Number:number,Channel:ch}
    t.mu.Unlock()
    cancel := func(){ t.mu.Lock();delete(t.qualifyWaiters,id);t.mu.Unlock() }
    select {
    case t.qualifyRequests <- QualifyRequest{ActionID:id, Number:number}:
        return ch,cancel,nil
    default:
        cancel()
        return nil,nil,errors.New("AMI qualify queue full")
    }
}
func (t *RegistrationTracker) QualifyRequests() <-chan QualifyRequest {
    if t==nil{return nil}
    return t.qualifyRequests
}
func (t *RegistrationTracker) OnQualifyResult(m map[string]string) {
    if t==nil || !strings.EqualFold(m["event"],"LinpushQualifyResult") { return }
    id := m["actionid"]
    t.mu.Lock()
    waiter,ok := t.qualifyWaiters[id]
    if ok && waiter.Number == m["aor"] {
        delete(t.qualifyWaiters,id)
        // Require both a fresh correlated OPTIONS response and an actual SIP 2xx.
        // Asterisk may otherwise classify 4xx as Reachable in legacy settings.
        waiter.Channel <- strings.EqualFold(m["status"],"Reachable") && strings.EqualFold(m["sip2xx"],"yes")
    }
    t.mu.Unlock()
}
func (t *RegistrationTracker) HasContact(number string) bool {
    if t==nil {return false}
    t.mu.Lock()
    defer t.mu.Unlock()
    return t.ready && t.connected && len(t.contacts[number])>0
}
func (t *RegistrationTracker) RevisionAndChange(number string) (uint64,<-chan struct{}) {
    t.mu.Lock()
    defer t.mu.Unlock()
    return t.revision[number],t.changed
}
