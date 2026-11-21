package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"example.org/asterisk-linphone-push/internal/ami"
	"example.org/asterisk-linphone-push/internal/metrics"
	"example.org/asterisk-linphone-push/internal/push"
	"example.org/asterisk-linphone-push/internal/server"
	"example.org/asterisk-linphone-push/internal/store"
)

type Config struct {
	Listen                string     `json:"listen"`
	Socket                string     `json:"unix_socket"`
	SocketGroup           string     `json:"socket_group"`
	Database              string     `json:"database"`
	ServiceAccountFile    string     `json:"service_account_file"`
	APITokenFile          string     `json:"api_token_file"`
	MetricsTokenFile      string     `json:"metrics_token_file"`
	FirebaseProjectID     string     `json:"firebase_project_id"`
	FirebaseProjectNumber string     `json:"firebase_project_number"`
	DefaultWaitSeconds    int        `json:"default_wait_seconds"`
	MaxWaitSeconds        int        `json:"max_wait_seconds"`
	FCMTTLSeconds         int        `json:"fcm_ttl_seconds"`
	AMIEnabled            bool       `json:"ami_enabled"`
	AMI                   ami.Config `json:"ami"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "wake":
		err = wake(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, "Usage: linpush serve --config /etc/linpush/config.json | linpush wake --number 101 OR --numbers 5:10:11 --wait 5 [--socket ...]")
}
func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9099"
	}
	if c.Socket == "" {
		c.Socket = "/run/linpush/linpush.sock"
	}
	if c.SocketGroup == "" {
		c.SocketGroup = "asterisk"
	}
	if c.Database == "" {
		c.Database = "/var/lib/linpush/linpush.sqlite3"
	}
	if c.DefaultWaitSeconds < 0 || c.MaxWaitSeconds <= 0 || c.MaxWaitSeconds > 60 || c.DefaultWaitSeconds > c.MaxWaitSeconds {
		return c, errors.New("default/max wait seconds invalid (max 1..60)")
	}
	if c.FCMTTLSeconds <= 0 {
		c.FCMTTLSeconds = 45
	}
	if c.ServiceAccountFile == "" || c.APITokenFile == "" {
		return c, errors.New("service_account_file and api_token_file required")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return c, errors.New("HTTP API must bind to loopback; use a TLS reverse proxy for remote access")
	}
	if c.AMIEnabled {
		if c.AMI.Address == "" {
			c.AMI.Address = "127.0.0.1:5038"
		}
		if c.AMI.Username == "" || c.AMI.PasswordFile == "" {
			return c, errors.New("AMI username and password_file required")
		}
		if c.AMI.IntervalSeconds == 0 {
			c.AMI.IntervalSeconds = 60
		}
		if c.AMI.IntervalSeconds < 10 || c.AMI.IntervalSeconds > 3600 {
			return c, errors.New("AMI interval must be 10..3600 seconds")
		}
		addr, _, err := net.SplitHostPort(c.AMI.Address)
		if err != nil || net.ParseIP(addr) == nil || !net.ParseIP(addr).IsLoopback() {
			return c, errors.New("AMI must use loopback (plaintext protocol)")
		}
		for _, n := range c.AMI.SingleContactAORs {
			if n == "" || len(n) > 16 || strings.Trim(n, "0123456789") != "" {
				return c, errors.New("invalid AMI single_contact_aors entry")
			}
		}
	}
	return c, nil
}
func serve(args []string) error {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfg := f.String("config", "/etc/linpush/config.json", "configuration JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	syscall.Umask(0077)
	c, err := loadConfig(*cfg)
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(c.ServiceAccountFile)
	if err != nil {
		return err
	}
	fcm, err := push.New(secret, c.FirebaseProjectID)
	if err != nil {
		return err
	}
	tokenBytes, err := os.ReadFile(c.APITokenFile)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(tokenBytes))
	if len(token) < 32 {
		return errors.New("API token must be at least 32 characters")
	}
	db, err := store.Open(c.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	metricsToken := ""
	if c.MetricsTokenFile != "" {
		raw, err := os.ReadFile(c.MetricsTokenFile)
		if err != nil {
			return fmt.Errorf("read metrics token file: %w", err)
		}
		metricsToken = strings.TrimSpace(string(raw))
		if len(metricsToken) < 32 {
			return errors.New("metrics token must be at least 32 characters")
		}
		if metricsToken == token {
			return errors.New("metrics token must differ from API admin token")
		}
	}
	reg := metrics.New()
	s := &server.Server{DB: db, FCM: fcm, APIToken: token, ProjectNumber: c.FirebaseProjectNumber,
		DefaultWait: c.DefaultWaitSeconds, MaxWait: c.MaxWaitSeconds, TTL: c.FCMTTLSeconds, Metrics: reg, MetricsToken: metricsToken}
	var syncer *ami.Synchronizer
	if c.AMIEnabled {
		secret, err := os.ReadFile(c.AMI.PasswordFile)
		if err != nil {
			return fmt.Errorf("read AMI password file: %w", err)
		}
		password := strings.TrimSpace(string(secret))
		if password == "" || strings.ContainsAny(password, "\r\n") {
			return errors.New("invalid AMI password file")
		}
		syncer = &ami.Synchronizer{Config: c.AMI, Password: password, ProjectNumber: c.FirebaseProjectNumber, DB: db}
		s.SyncStatus = func() any { return syncer.Status() }
		syncer.OnStatus = func(st ami.Status, event string) {
			reg.AMI(st.Connected, event == "success", event == "error", st.SeenContacts, st.FCMTokens)
		}
	}
	if err := os.MkdirAll(parent(c.Socket), 0755); err != nil {
		return err
	}
	if info, err := os.Lstat(c.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("socket path exists and is not a socket")
		}
		// Do not unlink a live service's socket.
		conn, e := net.DialTimeout("unix", c.Socket, 250*time.Millisecond)
		if e == nil {
			conn.Close()
			return errors.New("service socket already active")
		}
		if err := os.Remove(c.Socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	sock, err := net.Listen("unix", c.Socket)
	if err != nil {
		return err
	}
	defer os.Remove(c.Socket)
	defer sock.Close()
	if c.SocketGroup != "" {
		group, err := user.LookupGroup(c.SocketGroup)
		if err != nil {
			return fmt.Errorf("socket_group: %w", err)
		}
		gid, err := strconv.Atoi(group.Gid)
		if err != nil {
			return err
		}
		if err := os.Chown(c.Socket, -1, gid); err != nil {
			return fmt.Errorf("socket chgrp: %w", err)
		}
	}
	if err := os.Chmod(c.Socket, 0660); err != nil {
		return err
	}
	apiSrv := &http.Server{Addr: c.Listen, Handler: s.PublicHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 30 * time.Second}
	socketSrv := &http.Server{Handler: s.InternalHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 30 * time.Second}
	httpLn, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer httpLn.Close()
	failures := make(chan error, 2)
	go func() { failures <- apiSrv.Serve(httpLn) }()
	go func() { failures <- socketSrv.Serve(sock) }()
	log.Printf("linpush started HTTP=%s socket=%s", c.Listen, c.Socket)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if syncer != nil {
		go syncer.Run(ctx)
	}
	select {
	case <-ctx.Done():
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("listener failed: %v", err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(shutdown)
	_ = socketSrv.Shutdown(shutdown)
	return err
}
func parent(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return "."
	}
	return path[:i]
}
func wake(args []string) error {
	f := flag.NewFlagSet("wake", flag.ContinueOnError)
	number := f.String("number", "", "single extension to wake")
	numbers := f.String("numbers", "", "colon/comma separated extensions, e.g. 5:10:11")
	seconds := f.Int("wait", -1, "seconds after push (default server policy)")
	sock := f.String("socket", "/run/linpush/linpush.sock", "Unix socket path")
	callID := f.String("call-id", "", "matching SIP INVITE Call-ID, if available")
	fromURI := f.String("from-uri", "", "optional SIP From URI")
	displayName := f.String("display-name", "", "optional caller display name")
	if err := f.Parse(args); err != nil {
		return err
	}
	if (*number == "") == (*numbers == "") {
		return errors.New("provide exactly one of --number or --numbers")
	}
	if *seconds < -1 {
		return errors.New("--wait cannot be negative")
	}
	var ptr *int
	if *seconds >= 0 {
		ptr = seconds
	}
	req := server.WakeRequest{Number: *number, WaitSeconds: ptr, CallID: *callID, FromURI: *fromURI, DisplayName: *displayName}
	if *numbers != "" {
		ns, err := parseNumberList(*numbers)
		if err != nil {
			return err
		}
		req.Numbers = ns
	}
	b, _ := json.Marshal(req)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", *sock)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 75 * time.Second, Transport: transport}
	httpReq, err := http.NewRequest(http.MethodPost, "http://unix/wake", bytes.NewReader(b))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("service HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if *numbers != "" {
		var res server.BatchWakeResult
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return err
		}
		fmt.Printf("%s targets=%d sent=%d failed=%d invalidated=%d waited=%ds\n", res.Status, len(res.Numbers), res.Sent, res.Failed, res.Invalidated, res.WaitSeconds)
		if res.Status == "send_failed" {
			return errors.New("FCM rejected all device sends")
		}
		return nil
	}
	var res server.WakeResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	fmt.Printf("%s sent=%d failed=%d invalidated=%d waited=%ds\n", res.Status, res.Sent, res.Failed, res.Invalidated, res.WaitSeconds)
	if res.Status == "send_failed" {
		return errors.New("FCM rejected all device sends")
	}
	return nil
}

var cliNumberPattern = regexp.MustCompile(`^[0-9]{1,16}$`)

func parseNumberList(v string) ([]string, error) {
	if len(v) > 600 {
		return nil, errors.New("too many targets")
	}
	v = strings.ReplaceAll(v, ":", ",")
	parts := strings.Split(v, ",")
	if len(parts) > server.MaxBatchTargets {
		return nil, fmt.Errorf("too many targets (max %d)", server.MaxBatchTargets)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if !cliNumberPattern.MatchString(p) {
			return nil, errors.New("--numbers must contain only 1-16 digit numbers separated by : or ,")
		}
		out = append(out, p)
	}
	return out, nil
}
