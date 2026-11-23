package ami

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.org/asterisk-linphone-push/internal/store"
)

type Config struct {
	Address           string   `json:"address"`
	Username          string   `json:"username"`
	PasswordFile      string   `json:"password_file"`
	IntervalSeconds   int      `json:"sync_interval_seconds"`
	SingleContactAORs []string `json:"single_contact_aors"`
}

type Status struct {
	Connected    bool   `json:"connected"`
	LastSuccess  string `json:"last_success,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	SeenContacts int    `json:"seen_contacts"`
	FCMTokens    int    `json:"fcm_tokens"`
}
type Synchronizer struct {
	Config        Config
	Password      string
	ProjectNumber string
	DB            *store.Store
	Registration  *RegistrationTracker
	mu            sync.RWMutex
	status        Status
	OnStatus      func(Status, string)
}

func (s *Synchronizer) Status() Status { s.mu.RLock(); defer s.mu.RUnlock(); return s.status }
func (s *Synchronizer) connected(on bool) {
	s.mu.Lock()
	s.status.Connected = on
	state := s.status
	s.mu.Unlock()
	if s.OnStatus != nil {
		s.OnStatus(state, "connection")
	}
}
func (s *Synchronizer) failure(err error) {
	s.mu.Lock()
	s.status.Connected = false
	s.status.LastError = err.Error()
	state := s.status
	s.mu.Unlock()
	if s.OnStatus != nil {
		s.OnStatus(state, "error")
	}
}
func (s *Synchronizer) success(total, found int) {
	s.mu.Lock()
	s.status.LastSuccess = time.Now().UTC().Format(time.RFC3339)
	s.status.LastError = ""
	s.status.SeenContacts = total
	s.status.FCMTokens = found
	state := s.status
	s.mu.Unlock()
	if s.OnStatus != nil {
		s.OnStatus(state, "success")
	}
}

// Run reconnects with bounded backoff. No token is removed when AMI is unavailable.
func (s *Synchronizer) Run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		s.failure(err)
		if s.Registration != nil { s.Registration.Disconnected() }
		// Never include URI, FCM token or raw AMI response in logs.
		log.Printf("AMI disconnected: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

type message map[string]string

func readPacket(r *bufio.Reader) (message, error) {
	m := make(message)
	total := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		total += len(line)
		if len(line) > 32768 || total > 131072 {
			return nil, errors.New("AMI message exceeds size limit")
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(m) == 0 {
				continue
			}
			return m, nil
		}
		sep := strings.IndexByte(line, ':')
		if sep < 1 {
			return nil, errors.New("invalid AMI line")
		}
		k := strings.ToLower(strings.TrimSpace(line[:sep]))
		v := strings.TrimSpace(line[sep+1:])
		m[k] = v
	}
}
func writeAction(conn net.Conn, headers ...string) error {
	for _, v := range headers {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("AMI header injection prevented")
		}
	}
	_, err := io.WriteString(conn, strings.Join(headers, "\r\n")+"\r\n\r\n")
	return err
}
func reader(conn net.Conn) <-chan message {
	ch := make(chan message, 64)
	go func() {
		defer close(ch)
		r := bufio.NewReaderSize(conn, 4096)
		_, err := r.ReadString('\n') // manager greeting
		if err != nil {
			return
		}
		for {
			m, err := readPacket(r)
			if err != nil {
				return
			}
			ch <- m
		}
	}()
	return ch
}
func take(ctx context.Context, ch <-chan message, timeout time.Duration) (message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(timeout):
		return nil, errors.New("AMI response timeout")
	case m, ok := <-ch:
		if !ok {
			return nil, errors.New("AMI connection closed")
		}
		return m, nil
	}
}

func (s *Synchronizer) session(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", s.Config.Address)
	if err != nil {
		return errors.New("cannot connect to AMI")
	}
	defer conn.Close()
	ch := reader(conn)
	if err := writeAction(conn, "Action: Login", "Username: "+s.Config.Username, "Secret: "+s.Password, "Events: on"); err != nil {
		return errors.New("AMI login write failed")
	}
	msg, err := take(ctx, ch, 8*time.Second)
	if err != nil {
		return err
	}
	if msg["response"] != "Success" {
		return errors.New("AMI login rejected; check credentials and permit settings")
	}
	s.connected(true)
	// Refresh on startup; refresh again on status changes and periodically.
	if err := s.snapshot(ctx, conn, ch, 1); err != nil {
		return err
	}
	interval := time.Duration(s.Config.IntervalSeconds) * time.Second
	if interval == 0 {
		interval = 60 * time.Second
	}
	next := time.NewTimer(interval)
	defer next.Stop()
	pending := time.NewTimer(time.Hour)
	if !pending.Stop() {
		select {
		case <-pending.C:
		default:
		}
	}
	defer pending.Stop()
	waiting := false
	serial := 2
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-ch:
			if !ok {
				return errors.New("AMI socket closed")
			}
			if m["event"] == "ContactStatus" {
                if s.Registration != nil { s.Registration.OnContactStatus(m) }
				if !waiting {
					pending.Reset(500 * time.Millisecond)
					waiting = true
				}
			}
		case <-s.Registration.RefreshRequests():
            if err := s.snapshot(ctx, conn, ch, serial); err != nil { return err }
            serial++
		case <-next.C:
			if err := s.snapshot(ctx, conn, ch, serial); err != nil {
				return err
			}
			serial++
			next.Reset(interval)
		case <-pending.C:
			waiting = false
			if err := s.snapshot(ctx, conn, ch, serial); err != nil {
				return err
			}
			serial++
			if !next.Stop() {
				select {
				case <-next.C:
				default:
				}
			}
			next.Reset(interval)
		}
	}
}
func (s *Synchronizer) snapshot(ctx context.Context, conn net.Conn, ch <-chan message, serial int) error {
	actionID := "linpush-" + strconv.Itoa(serial)
	if err := writeAction(conn, "Action: PJSIPShowContacts", "ActionID: "+actionID); err != nil {
		return errors.New("AMI snapshot write failed")
	}
	contacts := make([]store.AutoContact, 0, 8)
    rawContacts := make([]map[string]string, 0, 8)
	total := 0
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errors.New("AMI ContactListComplete timeout (discarding partial snapshot)")
		case m, ok := <-ch:
			if !ok {
				return errors.New("AMI connection closed during ContactList")
			}
			if m["actionid"] != "" && m["actionid"] != actionID {
				continue
			}
			if m["response"] == "Error" {
				return errors.New("AMI PJSIPShowContacts denied or unavailable")
			}
			switch m["event"] {
			case "ContactStatus":
                if s.Registration != nil { s.Registration.OnContactStatus(m) }
			case "ContactList":
				rawContacts = append(rawContacts, m)
				total++
				if total > 10000 {
					return errors.New("AMI contact list limit exceeded")
				}
				c, valid := ParseContact(m, s.ProjectNumber)
				if valid {
					contacts = append(contacts, c)
				}
			case "ContactListComplete":
				if nstr := m["listitems"]; nstr != "" {
					n, err := strconv.Atoi(nstr)
					if err != nil || n != total {
						return fmt.Errorf("AMI snapshot incomplete: got %d; expected %s", total, nstr)
					}
				}
				singles := make(map[string]bool)
				for _, aor := range s.Config.SingleContactAORs {
					singles[aor] = true
				}
				if err := s.DB.ApplyAMISnapshot(contacts, singles); err != nil {
					return errors.New("SQLite AMI snapshot update failed")
				}
				if s.Registration != nil { s.Registration.ApplySnapshot(rawContacts, s.ProjectNumber) }
                s.success(total, len(contacts))
				log.Printf("AMI sync OK: %d contacts, %d FCM contacts", total, len(contacts))
				return nil
			}
		}
	}
}
