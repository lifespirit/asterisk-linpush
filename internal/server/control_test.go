package server

import (
    "bytes"
    "context"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "example.org/asterisk-linphone-push/internal/ami"
)

func TestUnixControl(t *testing.T) {
    srv := newBatchServer(t, &dummy{})
    handler := srv.InternalHandler()
    check := func(method, path, payload string, want int, status string) {
        t.Helper()
        req := httptest.NewRequest(method, path, bytes.NewBufferString(payload))
        rec := httptest.NewRecorder()
        handler.ServeHTTP(rec, req)
        if rec.Code != want || (status != "" && !strings.Contains(rec.Body.String(), status)) {
            t.Fatalf("%s %s => %d %s", method, path, rec.Code, rec.Body.String())
        }
    }
    check("GET", "/control/5", "", 200, "disabled")
    check("POST", "/control/5", `{"enabled":true}`, 200, "enabled")
    check("GET", "/control/5", "", 200, "enabled")
    check("POST", "/control/5", `{"enabled":false}`, 200, "disabled")
    check("POST", "/control/5", `{"enabled":"oops"}`, 400, "")
    check("POST", "/control/5;evil", `{"enabled":true}`, 400, "")
}

// TestWaitRegisterAfterSend exercises the real waiter with the existing
// dummy FCM sender, including an AMI event arriving while the CLI is blocked.
func TestWaitRegisterAfterSend(t *testing.T) {
    srv := newBatchServer(t, &dummy{})
    addBatch(t, srv, "5", "sample_fcm_token_12345678901234567890")
    tr := ami.NewRegistrationTracker()
    tr.ApplySnapshot(nil, "")
    srv.Registration = tr
    limit := 2
    go func() {
        time.Sleep(80*time.Millisecond)
        tr.OnContactStatus(map[string]string{"aor":"5", "contactstatus":"Updated", "uri":"sip:5@192.0.2.1;pn-provider=fcm"})
    }()
    start := time.Now()
    result, code, err := srv.Wake(context.Background(), WakeRequest{Number:"5",WaitRegisterSeconds:&limit})
    if err != nil || code != 200 || result.Registration != "registered" {
        t.Fatalf("failed to release on REGISTER: %+v %d %v", result,code,err)
    }
    if time.Since(start) > time.Second { t.Fatal("wait did not release promptly") }
}
