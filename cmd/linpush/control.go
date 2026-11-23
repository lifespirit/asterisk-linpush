package main

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "io"
    "net"
    "net/http"
    "time"
)

// Prints exactly "enabled" or "disabled" on stdout for Asterisk SHELL().
func control(args []string) error {
    f := flag.NewFlagSet("control", flag.ContinueOnError)
    sock := f.String("socket", "/run/linpush/linpush.sock", "Unix socket path")
    if err := f.Parse(args); err != nil { return err }
    if f.NArg() != 2 { return errors.New("usage: linpush control [status|enable|disable] NUMBER") }
    action, number := f.Arg(0), f.Arg(1)
    if !cliNumberPattern.MatchString(number) { return errors.New("invalid extension") }
    method := http.MethodGet
    var body io.Reader
    if action == "enable" || action == "disable" {
        method = http.MethodPost
        raw, _ := json.Marshal(map[string]bool{"enabled": action=="enable"})
        body = bytes.NewReader(raw)
    } else if action != "status" {
        return errors.New("control action must be status, enable, or disable")
    }
    transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
        return (&net.Dialer{Timeout: 3*time.Second}).DialContext(ctx, "unix", *sock)
    }}
    defer transport.CloseIdleConnections()
    client := &http.Client{Timeout: 5*time.Second, Transport: transport}
    req, err := http.NewRequest(method, "http://unix/control/"+number, body)
    if err != nil { return err }
    if method == http.MethodPost { req.Header.Set("Content-Type", "application/json") }
    resp, err := client.Do(req)
    if err != nil { return err }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("control HTTP %d", resp.StatusCode)
    }
    var result struct { Status string `json:"status"` }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil { return err }
    if result.Status != "enabled" && result.Status != "disabled" {
        return errors.New("invalid control response")
    }
    fmt.Println(result.Status)
    return nil
}
