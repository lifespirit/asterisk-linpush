package server

import (
    "context"
    "errors"
    "sync"
    "strings"
    "time"

    "example.org/asterisk-linphone-push/internal/ami"
)

// WakeReady is an opt-in path: existing wake and wait-register behavior remains
// unchanged. OPTIONS succeeds ONLY on a unique AMI LinpushQualifyResult ActionID.
// An unpatched Asterisk will fall back to FCM + REGISTER/timeout.
func (s *Server) wakeReadySingle(ctx context.Context, req WakeRequest) (WakeResult, int, error) {
    if !validNumber(req.Number) { return WakeResult{},400,errors.New("invalid extension number") }
    if err := s.validateReady(req); err!=nil { return WakeResult{},400,err }
    if s.Metrics != nil { s.Metrics.Start() }
    start:=time.Now()
    out,status,err := s.readySingle(ctx,req,req.Number,nil)
    label:="error"
    if err==nil {label=out.Status}
    s.observe("single",label,start,time.Since(start),1)
    return out,status,err
}

func (s *Server) wakeReadyMany(ctx context.Context,req WakeRequest) (BatchWakeResult,int,error) {
    numbers,err:=parseTargets(req)
    if err!=nil {return BatchWakeResult{},400,err}
    if err:=s.validateReady(req);err!=nil{return BatchWakeResult{},400,err}
    // Validate all targets before any SIP probe or FCM dispatch.
    if len(req.CallID)>150 || len(req.FromURI)>256 || len(req.DisplayName)>128 ||
        strings.ContainsAny(req.CallID+req.FromURI+req.DisplayName,"\r\n\x00"){
        return BatchWakeResult{},400,errors.New("invalid push metadata")
    }
    if s.Metrics != nil {s.Metrics.Start()}
    start:=time.Now()
    out:=BatchWakeResult{Numbers:numbers,Results:make([]WakeResult,len(numbers))}
    ctx,cancel:=context.WithTimeout(ctx,time.Duration(*req.WaitReadySeconds)*time.Second)
    defer cancel()
    var wg sync.WaitGroup
    var dedup sync.Map
    var firstErr error
    var firstStatus int
    var mu sync.Mutex
    for i,n := range numbers {
        wg.Add(1)
        go func(i int,n string) {
            defer wg.Done()
            one:=req
            one.Number=n
            one.Numbers=nil
            res,status,e:=s.readySingleWithContext(ctx,one,n,&dedup)
            mu.Lock()
            if e!=nil && firstErr==nil {firstErr=e;firstStatus=status}
            out.Results[i]=res
            mu.Unlock()
        }(i,n)
    }
    wg.Wait()
    if firstErr!=nil {
        s.observe("batch","error",start,time.Since(start),len(numbers))
        return BatchWakeResult{},firstStatus,firstErr
    }
    allReady:=true
    allSkipped:=true
    for _,one:=range out.Results {
        out.Sent+=one.Sent
        out.Failed+=one.Failed
        out.Invalidated+=one.Invalidated
        if one.Status!="disabled" && one.Status!="no_devices" && one.Status!="deduplicated" {
            allSkipped=false
            if one.Registration!="registered" && one.Registration!="options" {allReady=false}
        }
    }
    switch {
    case allSkipped:out.Status="skipped"
    case allReady:out.Status="ready"
    case out.Sent>0:out.Status="sent"
    case out.Failed>0:out.Status="send_failed"
    default:out.Status="skipped"
    }
    if out.Sent>0 || out.Status=="ready" {
        out.Registration="timeout"
        if allReady {out.Registration="ready"}
        out.WaitSeconds=int(time.Since(start).Seconds())
    }
    s.observe("batch",out.Status,start,time.Since(start),len(numbers))
    return out,200,nil
}

func (s *Server) validateReady(req WakeRequest) error {
    if req.WaitReadySeconds==nil || *req.WaitReadySeconds<=0 ||
        *req.WaitReadySeconds>s.MaxWait {return errors.New("invalid wait_ready_seconds")}
    if req.WaitSeconds!=nil {return errors.New("wait_seconds and wait_ready_seconds are mutually exclusive")}
    if req.WaitRegisterSeconds!=nil {return errors.New("wait_register_seconds and wait_ready_seconds are mutually exclusive")}
    if req.PushGraceMs!=nil && (*req.PushGraceMs<0 || *req.PushGraceMs>2000) {
        return errors.New("push_grace_ms must be 0..2000")
    }
    return nil
}
func (s *Server) readySingle(ctx context.Context,req WakeRequest,n string,dedup *sync.Map) (WakeResult,int,error) {
    ctx,cancel:=context.WithTimeout(ctx,time.Duration(*req.WaitReadySeconds)*time.Second)
    defer cancel()
    return s.readySingleWithContext(ctx,req,n,dedup)
}
func (s *Server) readySingleWithContext(ctx context.Context,req WakeRequest,n string,dedup *sync.Map) (WakeResult,int,error) {
    ext,exists,err:=s.DB.GetExtension(n)
    if err!=nil{return WakeResult{},500,errors.New("database query failed")}
    if !exists || !ext.PushEnabled{return WakeResult{Number:n,Status:"disabled"},200,nil}
    devices,err:=s.DB.ListDevices(n,true)
    if err!=nil{return WakeResult{},500,errors.New("database query failed")}
    if len(devices)==0{return WakeResult{Number:n,Status:"no_devices"},200,nil}

    baseline:=s.registrationBaseline([]string{n})
    grace:=300
    if req.PushGraceMs!=nil{grace=*req.PushGraceMs}
    hasContact:=s.Registration!=nil && s.Registration.HasContact(n)
    var probe <-chan bool
    var cancelProbe func()
    if hasContact {
        probe,cancelProbe,_=s.Registration.Probe(n)
    }
    if cancelProbe!=nil{defer func(){cancelProbe()}()}

    // Avoid sending FCM to a phone that already answers a fresh OPTIONS.
    if hasContact && grace>0 {
        delay:=time.NewTimer(time.Duration(grace)*time.Millisecond)
        defer delay.Stop()
        for {
            var changes <-chan struct{}
            if baseline!=nil {
                rev,changed:=s.Registration.RevisionAndChange(n)
                if rev>baseline[n] {
                    return WakeResult{Number:n,Status:"ready",Registration:"registered"},200,nil
                }
                changes=changed
            }
            select {
            case ok := <-probe:
                if ok {return WakeResult{Number:n,Status:"ready",Registration:"options"},200,nil}
                probe=nil
            case <-changes:
                continue
            case <-delay.C:
                goto sendPush
            case <-ctx.Done():
                return WakeResult{Number:n,Status:"ready",Registration:"timeout"},200,nil
            }
        }
    }
sendPush:
    zero:=0
    outgoing:=req
    outgoing.Number=n
    outgoing.Numbers=nil
    outgoing.WaitSeconds=&zero
    outgoing.WaitReadySeconds=nil
    outgoing.WaitRegisterSeconds=nil
    result,status,err:=s.wakeOne(ctx,outgoing,dedup)
    if err!=nil{return result,status,err}
    if result.Sent==0 {return result,status,nil}

    // One deadline for the complete operation, including the initial grace.
    // Do not use the Asterisk cached Reachable status as new evidence.
    ticker:=time.NewTicker(time.Second)
    defer ticker.Stop()
    if hasContact && probe==nil {
        if cancelProbe!=nil{cancelProbe()}
        probe,cancelProbe,_=s.Registration.Probe(n)
    }
    for {
        var changes <-chan struct{}
        if baseline!=nil {
            rev,changed:=s.Registration.RevisionAndChange(n)
            if rev>baseline[n] {
                result.Registration="registered"
                return result,status,nil
            }
            changes=changed
        }
        select {
        case ok:=<-probe:
            if ok {
                result.Registration="options"
                return result,status,nil
            }
            probe=nil
        case <-changes:
            continue
        case <-ticker.C:
            if s.Registration!=nil && s.Registration.HasContact(n) && probe==nil {
                if cancelProbe!=nil{cancelProbe()}
                probe,cancelProbe,_=s.Registration.Probe(n)
            }
        case <-ctx.Done():
            result.Registration="timeout"
            return result,status,nil
        }
    }
}

// Keeping this compile-time reference intentional: the AMI type must remain a
// dependency of this layer, never replaced by saved contacts from SQLite.
var _ *ami.RegistrationTracker
