package ami

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.org/asterisk-linphone-push/internal/store"
)

func TestParseContact(t *testing.T) {
	token := "efN6lQABC:APA91bFTnOwTK_2kmlHHXzH5f-KZCyKq1234567673LoZtD7_-SBHj45tGsQNnnUUxhrL00fSxZHZQWySEsw1BybaRvuMJBxrA6x-Eh"
	m := map[string]string{"objectname": "5@@abcde", "endpoint": "5", "uri": "sip:5@109.248.132.249:49022;transport=tls;pn-prid=" + token + ";pn-provider=fcm;pn-param=750067000448;pn-silent=1;pn-timeout=0"}
	c, ok := ParseContact(m, "750067000448")
	if !ok || c.Number != "5" || c.Token != token || c.PNParam != "750067000448" || !strings.HasPrefix(c.ID, "auto-") {
		t.Fatalf("contact parse failed: %+v %v", c, ok)
	}
	if _, ok := ParseContact(m, "wrong-project"); ok {
		t.Fatal("accepted wrong firebase project")
	}
	m["uri"] = "sip:5@host;transport=tls"
	if _, ok := ParseContact(m, ""); ok {
		t.Fatal("accepted no pn-prid")
	}
	m["uri"] = "sip:5@host;pn-prid=" + token + ";pn-provider=fcm;pn-param=750067000448"
	m["endpoint"] = "4"
	if _, ok := ParseContact(m, ""); ok {
		t.Fatal("accepted different endpoint")
	}
}

func TestSessionIntegrationAndIncompleteSnapshot(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.SetEnabled("5", true)
	_ = db.UpsertDevice("5", "fallback", "abcabcabcabcabcabcabcabcabcabcabc", "")
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "Asterisk Call Manager/13.0.0\r\n")
		reader := bufio.NewReader(conn)
		snapshots := 0
		for {
			msg, err := readPacket(reader)
			if err != nil {
				return
			}
			switch msg["action"] {
			case "Login":
				fmt.Fprint(conn, "Response: Success\r\nMessage: Authentication accepted\r\n\r\n")
			case "PJSIPShowContacts":
				snapshots++
				fmt.Fprintf(conn, "Response: Success\r\nActionID: %s\r\nEventList: start\r\n\r\n", msg["actionid"])
				if snapshots == 1 {
					// Seven contacts as seen on hrouter: only #5 has FCM metadata.
					for _, n := range []string{"10", "11", "1", "2", "3", "4", "5"} {
						uri := "sip:" + n + "@1.2.3.4:4444;transport=tls"
						if n == "5" {
							uri += ";pn-provider=fcm;pn-prid=token0123456789abcdefghijklmno;pn-param=750067000448"
						}
						fmt.Fprintf(conn, "Event: ContactList\r\nActionID: %s\r\nObjectName: %s@@abc\r\nEndpoint: %s\r\nUri: %s\r\n\r\n", msg["actionid"], n, n, uri)
					}
					fmt.Fprintf(conn, "Event: ContactListComplete\r\nActionID: %s\r\nListItems: 7\r\n\r\n", msg["actionid"])
					fmt.Fprint(conn, "Event: ContactStatus\r\nContactStatus: Updated\r\nAOR: 5\r\nURI: sip:5@1.2.3.4\r\n\r\n")
				} else {
					// Incomplete snapshot with new token: MUST be rejected atomically.
					fmt.Fprintf(conn, "Event: ContactList\r\nActionID: %s\r\nObjectName: 5@@abc\r\nEndpoint: 5\r\nUri: sip:5@1.2.3.4;pn-provider=fcm;pn-prid=newtoken0123456789abcdefghijklmno;pn-param=750067000448\r\n\r\n", msg["actionid"])
					fmt.Fprintf(conn, "Event: ContactListComplete\r\nActionID: %s\r\nListItems: 2\r\n\r\n", msg["actionid"])
					return
				}
			}
		}
	}()
	syncer := &Synchronizer{Config: Config{Address: l.Addr().String(), Username: "linpush", IntervalSeconds: 60, SingleContactAORs: []string{"5"}}, Password: "fake-secret", ProjectNumber: "750067000448", DB: db}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = syncer.session(ctx)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected an incomplete-snapshot error, got %v", err)
	}
	status := syncer.Status()
	if status.SeenContacts != 7 || status.FCMTokens != 1 || status.LastSuccess == "" {
		t.Fatalf("incomplete synchronization: %+v", status)
	}
	ex, ok, err := db.GetExtension("5")
	if err != nil || !ok || !ex.PushEnabled || ex.Devices != 2 {
		t.Fatalf("preferences were overwritten: %+v %v", ex, err)
	}
	ds, err := db.ListDevices("5", true)
	if err != nil || len(ds) != 2 {
		t.Fatalf("bad devices: %+v %v", ds, err)
	}
	for _, d := range ds {
		if strings.HasPrefix(d.Token, "newtoken") {
			t.Fatal("partial snapshot was committed")
		}
	}
}
