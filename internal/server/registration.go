package server

import (
    "context"
    "time"
)

func (s *Server) registrationBaseline(numbers []string) map[string]uint64 {
    if s.Registration == nil { return nil }
    baseline, ok := s.Registration.Baseline(numbers)
    if !ok { return nil }
    return baseline
}

// registrationWait uses one shared deadline regardless of target count.
// The observed registration is a new SIP REGISTER, not an OPTIONS probe.
func (s *Server) registrationWait(ctx context.Context, baseline map[string]uint64, numbers []string, seconds int) (map[string]bool, int) {
    if len(numbers) == 0 { return map[string]bool{}, 0 }
    start := time.Now()
    ready := s.Registration.Wait(ctx, baseline, numbers, time.Duration(seconds)*time.Second)
    elapsed := time.Since(start)
    rounded := int(elapsed / time.Second)
    if elapsed % time.Second != 0 && elapsed > 0 { rounded++ }
    if rounded > seconds { rounded = seconds }
    all := true
    for _, n := range numbers { if !ready[n] { all = false; break } }
    if s.Metrics != nil {
        if all { s.Metrics.Registration("registered", elapsed) } else { s.Metrics.Registration("timeout", elapsed) }
    }
    return ready, rounded
}
