package ami

import (
    "testing"
    "time"
)

func TestQualifyResultIsCorrelated(t *testing.T) {
    tr:=NewRegistrationTracker()
    tr.ApplySnapshot(nil,"")
    ch,cancel,err:=tr.Probe("5")
    if err!=nil{t.Fatal(err)}
    defer cancel()
    q:=<-tr.QualifyRequests()
    if q.Number!="5" || q.ActionID=="" {t.Fatalf("invalid request %+v",q)}
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":"wrong","aor":"5","status":"Reachable","sip2xx":"yes"})
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"6","status":"Reachable","sip2xx":"yes"})
    select{case <-ch:t.Fatal("unmatched probe was accepted");default:}
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"5","status":"Reachable","sip2xx":"yes"})
    select {
    case ready:=<-ch:if !ready{t.Fatal("matched OPTIONS rejected")}
    case <-time.After(time.Second):t.Fatal("matched OPTIONS not released")
    }
}
func TestQualifyFailureAndNoStaleState(t *testing.T) {
    tr:=NewRegistrationTracker()
    tr.ApplySnapshot(nil,"")
    ch,stop,err:=tr.Probe("5")
    if err!=nil{t.Fatal(err)}
    defer stop()
    req:=<-tr.QualifyRequests()
    tr.OnContactStatus(map[string]string{"event":"ContactStatus","aor":"5","contactstatus":"Reachable","uri":"sip:5@host;pn-provider=fcm"})
    select {case <-ch:t.Fatal("cached contact status accepted");default:}
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":req.ActionID,"aor":"5","status":"Unreachable","sip2xx":"no"})
    select {
    case value:=<-ch:if value {t.Fatal("failed probe returned success")}
    case <-time.After(time.Second):t.Fatal("failed probe not delivered")
    }
}
