package ami

import (
    "testing"
    "time"
)

func TestQualifyResultIsCorrelated(t *testing.T) {
    tr:=NewRegistrationTracker()
    tr.ApplySnapshot([]map[string]string{{
        "objectname":"5@@fcm-one",
        "uri":"sip:5@192.0.2.10;pn-provider=fcm;pn-prid=valid_fcm_token_1234567890",
        "endpoint":"5",
    }},"")
    ch,cancel,err:=tr.Probe("5")
    if err!=nil{t.Fatal(err)}
    defer cancel()
    q:=<-tr.QualifyRequests()
    if q.Number!="5" || q.ActionID=="" {t.Fatalf("invalid request %+v",q)}
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":"wrong","aor":"5","contactid":"5@@fcm-one","status":"Reachable","sip2xx":"yes"})
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"6","status":"Reachable","sip2xx":"yes"})
    select{case <-ch:t.Fatal("unmatched probe was accepted");default:}
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"5","contactid":"5@@not-our-fcm","status":"Reachable","sip2xx":"yes"})
    select { case <-ch: t.Fatal("wrong contact ID accepted"); default: }
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"5","contactid":"5@@fcm-one","status":"Reachable","sip2xx":"yes"})
    select {
    case ready:=<-ch:if !ready{t.Fatal("matched OPTIONS rejected")}
    case <-time.After(time.Second):t.Fatal("matched OPTIONS not released")
    }
}
func TestQualifyFailureAndNoStaleState(t *testing.T) {
    tr:=NewRegistrationTracker()
    tr.ApplySnapshot([]map[string]string{{
        "objectname":"5@@fcm-one",
        "uri":"sip:5@192.0.2.10;pn-provider=fcm;pn-prid=valid_fcm_token_1234567890",
        "endpoint":"5",
    }},"")
    ch,stop,err:=tr.Probe("5")
    if err!=nil{t.Fatal(err)}
    defer stop()
    req:=<-tr.QualifyRequests()
    tr.OnContactStatus(map[string]string{"event":"ContactStatus","aor":"5","contactstatus":"Reachable","uri":"sip:5@host;pn-provider=fcm"})
    select {case <-ch:t.Fatal("cached contact status accepted");default:}
    tr.OnQualifyResult(map[string]string{"event":"LinpushQualifyResult","actionid":req.ActionID,"aor":"5","contactid":"5@@fcm-one","status":"Unreachable","sip2xx":"no"})
    select {
    case value:=<-ch:if value {t.Fatal("failed probe returned success")}
    case <-time.After(time.Second):t.Fatal("failed probe not delivered")
    }
}

func TestQualifyRejectsNon2xxAndWrongContact(t *testing.T) {
    tr:=NewRegistrationTracker()
    tr.ApplySnapshot([]map[string]string{{
        "objectname":"5@@fcm-one",
        "uri":"sip:5@192.0.2.10;pn-provider=fcm;pn-prid=valid_fcm_token_1234567890",
        "endpoint":"5",
    }},"")
    ch,cancel,err:=tr.Probe("5")
    if err!=nil {t.Fatal(err)}
    defer cancel()
    q:=<-tr.QualifyRequests()
    tr.OnQualifyResult(map[string]string{
        "event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"5",
        "contactid":"5@@fcm-one","status":"Reachable","sip2xx":"no",
    })
    select {
    case success:=<-ch:
        if success {t.Fatal("Reachable without SIP 2xx accepted")}
    case <-time.After(time.Second):t.Fatal("non-2xx response was not recorded")
    }
}
func TestQualifySeveralFCMContacts(t *testing.T) {
    tr:=NewRegistrationTracker()
    tr.ApplySnapshot([]map[string]string{
        {"objectname":"5@@a","endpoint":"5","uri":"sip:5@192.0.2.1;pn-provider=fcm;pn-prid=valid_fcm_token_1234567890"},
        {"objectname":"5@@b","endpoint":"5","uri":"sip:5@192.0.2.2;pn-provider=fcm;pn-prid=valid_fcm_token_1234567891"},
    },"")
    ch,cancel,err:=tr.Probe("5")
    if err!=nil {t.Fatal(err)}
    defer cancel()
    q:=<-tr.QualifyRequests()
    tr.OnQualifyResult(map[string]string{
        "event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"5",
        "contactid":"5@@a","status":"Unreachable","sip2xx":"no",
    })
    select {case <-ch:t.Fatal("first failed contact finished multi-contact qualify");default:}
    tr.OnQualifyResult(map[string]string{
        "event":"LinpushQualifyResult","actionid":q.ActionID,"aor":"5",
        "contactid":"5@@b","status":"Reachable","sip2xx":"yes",
    })
    select {
    case success:=<-ch:if !success {t.Fatal("second successful contact ignored")}
    case <-time.After(time.Second):t.Fatal("successful second contact missing")
    }
}
