package store

import (
	"path/filepath"
	"testing"
)

func TestPersistenceAndDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.sqlite3")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDevice("101", "phone-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaa", "987654321"); err != nil {
		t.Fatal(err)
	}
	x, exists, err := db.GetExtension("101")
	if err != nil || !exists || x.PushEnabled {
		t.Fatalf("implicit extension should be disabled: %+v %v", x, err)
	}
	if err := db.SetEnabled("101", true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertDevice("101", "phone-b", "bbbbbbbbbbbbbbbbbbbbbbbbbbbb", "987654321"); err != nil {
		t.Fatal(err)
	}
	if err := db.DisableDevice("101", "phone-a", "UNREGISTERED"); err != nil {
		t.Fatal(err)
	}
	active, err := db.ListDevices("101", true)
	if err != nil || len(active) != 1 || active[0].ID != "phone-b" {
		t.Fatalf("active devices: %+v %v", active, err)
	}
	all, err := db.ListDevices("101", false)
	if err != nil || len(all) != 2 || all[0].LastError != "UNREGISTERED" {
		t.Fatalf("all devices: %+v %v", all, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	x, exists, err = db.GetExtension("101")
	if err != nil || !exists || !x.PushEnabled || x.Devices != 1 {
		t.Fatalf("persistence: %+v %v", x, err)
	}
	if err := db.UpsertDevice("101", "phone-a", "cccccccccccccccccccccccccc", "987654321"); err != nil {
		t.Fatal(err)
	}
	x, _, _ = db.GetExtension("101")
	if x.Devices != 2 {
		t.Fatalf("upsert did not restore device: %+v", x)
	}
	if err := db.RemoveDevice("101", "phone-b"); err != nil {
		t.Fatal(err)
	}
	x, _, _ = db.GetExtension("101")
	if x.Devices != 1 {
		t.Fatalf("delete failed: %+v", x)
	}
}

func TestAutoSnapshotMigrationAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent.sqlite3")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetEnabled("5", true)
	_ = db.UpsertDevice("5", "emergency", "manualManualManualManual012345", "")
	contacts := []AutoContact{{Number: "5", ID: "auto-one", Token: "token_token_token_12345678901", PNParam: "123", ContactHash: "abc"}}
	singles := map[string]bool{"5": true}
	if err := db.ApplyAMISnapshot(contacts, singles); err != nil {
		t.Fatal(err)
	}
	if err := db.DisableDevice("5", "auto-one", "UNREGISTERED"); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyAMISnapshot(contacts, singles); err != nil {
		t.Fatal(err)
	}
	d, err := db.ListDevices("5", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 || d[0].Active {
		t.Fatalf("invalid token was incorrectly revived: %+v", d)
	}
	// Missing FCM contacts must never delete or disable tokens.
	if err := db.ApplyAMISnapshot(nil, singles); err != nil {
		t.Fatal(err)
	}
	d, _ = db.ListDevices("5", false)
	if len(d) != 2 {
		t.Fatal("deleted token after expired registration")
	}
	// New FCM token supersedes old when max_contacts=1.
	contacts = []AutoContact{{Number: "5", ID: "auto-two", Token: "newtoken_token_987654321000000", PNParam: "123"}}
	if err := db.ApplyAMISnapshot(contacts, singles); err != nil {
		t.Fatal(err)
	}
	d, _ = db.ListDevices("5", true)
	if len(d) != 2 {
		t.Fatalf("should preserve manual and new auto: %+v", d)
	}
	for _, dev := range d {
		if dev.ID == "auto-one" {
			t.Fatal("stale token active")
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ext, _, err := db.GetExtension("5")
	if err != nil || !ext.PushEnabled {
		t.Fatal("lost previous preference")
	}
}
