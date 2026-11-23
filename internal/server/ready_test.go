package server

import (
    "context"
    "sync"
    "testing"
    "time"

    "example.org/asterisk-linphone-push/internal/ami"
    "example.org/asterisk-linphone-push/internal/push"
)
type readyFakeSender struct { mu sync.Mutex; count int }
func (r *readyFakeSender) Send(_ context.Context,_ string,_ push.Payload) error {
    r.mu.Lock();r.count++;r.mu.Unlock();return nil
}
func (r *readyFakeSender) Count() int {r.mu.Lock();defer r.mu.Unlock();return r.count}
func newReadyTestServer(t *testing.T) (*Server,*ami.RegistrationTracker,*readyFakeSender) {
    t.Helper()
    sender:=&readyFakeSender{}
    srv:=newBatchServer(t,sender)
    addBatch(t,srv,"5","validtoken_12345678901234567890123456789")
    tr:=ami.NewRegistrationTracker()
    tr.ApplySnapshot([]map[string]string{{
        "objectname":"5@@id","endpoint":"5",
        "uri":"sip:5@192.0.2.1;pn-provider=fcm;pn-prid=validtoken_12345678901234567890123456789",
        "expirationtime":"1800000000",
    }},"")
    srv.Registration=tr
    return srv,tr,sender
}
func TestReadyQualifyBeforeFCM(t *testing.T) {
    srv,tr,sender:=newReadyTestServer(t)
    secs,grace:=2,400
    go func() {
        q:=<-tr.QualifyRequests()
        time.Sleep(30*time.Millisecond)
        tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult",
            "actionid":q.ActionID,"aor":"5","status":"Reachable"})
    }()
    result,code,err:=srv.Wake(context.Background(),WakeRequest{
        Number:"5",WaitReadySeconds:&secs,PushGraceMs:&grace,
    })
    if err!=nil||code!=200||result.Registration!="options"||sender.Count()!=0 {
        t.Fatalf("fresh probe did not bypass FCM: %+v %d %v sends=%d",result,code,err,sender.Count())
    }
}
func TestReadyNoQualifyEventFallsBackToFCM(t *testing.T) {
    srv,_,sender:=newReadyTestServer(t)
    secs,grace:=1,30
    started:=time.Now()
    result,code,err:=srv.Wake(context.Background(),WakeRequest{
        Number:"5",WaitReadySeconds:&secs,PushGraceMs:&grace,
    })
    if err!=nil||code!=200||result.Registration!="timeout"||sender.Count()!=1{
        t.Fatalf("incorrect legacy fallback: %+v %d %v sends=%d",result,code,err,sender.Count())
    }
    if time.Since(started)<time.Duration(secs)*time.Second {
        t.Fatalf("wait released before deadline")
    }
}
func TestReadyRegistrationDuringWait(t *testing.T) {
    srv,tr,sender:=newReadyTestServer(t)
    secs,grace:=2,10
    go func() {
        time.Sleep(80*time.Millisecond)
        tr.OnContactStatus(map[string]string{"aor":"5","contactstatus":"Updated",
            "uri":"sip:5@192.0.2.1;pn-provider=fcm"})
    }()
    result,code,err:=srv.Wake(context.Background(),WakeRequest{
        Number:"5",WaitReadySeconds:&secs,PushGraceMs:&grace,
    })
    if err!=nil||code!=200||result.Registration!="registered"||sender.Count()!=1{
        t.Fatalf("new REGISTER not accepted: %+v %d %v sends=%d",result,code,err,sender.Count())
    }
}
