package ami

import (
    "context"
    "testing"
    "time"
)

const exampleURI = "sip:5@192.0.2.10:5061;pn-provider=fcm;pn-prid=testFCMtoken_1234567890123456789;pn-param=123"

func TestRegistrationTrackerIgnoresOptionsAndUsesUpdated(t *testing.T) {
    tr := NewRegistrationTracker()
    tr.ApplySnapshot(nil, "123")
    before, ok := tr.Baseline([]string{"5"})
    if !ok { t.Fatal("tracker not ready") }
    tr.OnContactStatus(map[string]string{"aor":"5","contactstatus":"Reachable","uri":exampleURI})
    got := tr.Wait(context.Background(), before, []string{"5"}, 10*time.Millisecond)
    if got["5"] { t.Fatal("OPTIONS marked a new registration") }
    tr.OnContactStatus(map[string]string{"aor":"5","contactstatus":"Updated","uri":exampleURI})
    got = tr.Wait(context.Background(), before, []string{"5"}, time.Second)
    if !got["5"] { t.Fatal("updated REGISTER not observed") }
}

func TestRegistrationTrackerSnapshotAndAllTargets(t *testing.T) {
    tr := NewRegistrationTracker()
    tr.ApplySnapshot(nil, "123")
    baseline, ok := tr.Baseline([]string{"5","10"})
    if !ok { t.Fatal("tracker not ready") }
    entry := map[string]string{"objectname":"5@@abc","endpoint":"5","uri":exampleURI,"expirationtime":"1800000000"}
    tr.ApplySnapshot([]map[string]string{entry}, "123")
    got := tr.Wait(context.Background(), baseline, []string{"5","10"}, 20*time.Millisecond)
    if !got["5"] || got["10"] { t.Fatalf("incorrect partial results: %+v", got) }
    tr.OnContactStatus(map[string]string{"aor":"10","contactstatus":"Updated","uri":"sip:10@192.0.2.11;pn-provider=fcm"})
    got = tr.Wait(context.Background(), baseline, []string{"5","10"}, time.Second)
    if !got["5"] || !got["10"] { t.Fatalf("expected both updates: %+v", got) }
}

func TestRegistrationTrackerRefreshExpirationAndDisconnect(t *testing.T) {
    tr := NewRegistrationTracker()
    entry := map[string]string{"objectname":"5@@abc","endpoint":"5","uri":exampleURI,"expirationtime":"1800000000"}
    tr.ApplySnapshot([]map[string]string{entry}, "123")
    baseline, ok := tr.Baseline([]string{"5"})
    if !ok { t.Fatal("initial snapshot not ready") }
    entry["expirationtime"]="1800000300"
    tr.ApplySnapshot([]map[string]string{entry}, "123")
    got := tr.Wait(context.Background(), baseline, []string{"5"}, time.Second)
    if !got["5"] { t.Fatal("new REGISTER expiration not detected") }
    tr.Disconnected()
    if _, ok := tr.Baseline([]string{"5"}); ok { t.Fatal("disconnected AMI returned stale baseline") }
}
