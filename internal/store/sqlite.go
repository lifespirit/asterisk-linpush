package store

/*
#cgo pkg-config: sqlite3
#include <stdlib.h>
#include <sqlite3.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"
)

type Store struct {
	mu sync.Mutex
	db *C.sqlite3
}

type Extension struct {
	Number      string `json:"number"`
	PushEnabled bool   `json:"push_enabled"`
	Devices     int    `json:"devices"`
	UpdatedAt   string `json:"updated_at"`
}

type AutoContact struct {
	Number      string
	ID          string
	Token       string
	PNParam     string
	ContactHash string
}

type Device struct {
	ID          string `json:"device_id"`
	Source      string `json:"source"`
	LastSeen    string `json:"last_seen,omitempty"`
	Token       string `json:"-"`
	TokenSuffix string `json:"token_suffix"`
	PNParam     string `json:"pn_param,omitempty"`
	Active      bool   `json:"active"`
	UpdatedAt   string `json:"updated_at"`
	LastError   string `json:"last_error,omitempty"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	var db *C.sqlite3
	rc := C.sqlite3_open_v2(cp, &db, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_FULLMUTEX, nil)
	if rc != C.SQLITE_OK {
		msg := "unknown"
		if db != nil {
			msg = C.GoString(C.sqlite3_errmsg(db))
			C.sqlite3_close(db)
		}
		return nil, fmt.Errorf("sqlite open: %s", msg)
	}
	s := &Store{db: db}
	if err := os.Chmod(path, 0600); err != nil {
		s.Close()
		return nil, err
	}
	stmts := []string{
		`PRAGMA foreign_keys=ON`, `PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS extensions (
            number TEXT PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 0,
            updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS devices (
            number TEXT NOT NULL REFERENCES extensions(number) ON DELETE CASCADE,
            device_id TEXT NOT NULL, token TEXT NOT NULL, pn_param TEXT NOT NULL DEFAULT '',
            active INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL,
            last_error TEXT NOT NULL DEFAULT '', PRIMARY KEY(number,device_id))`,
		`CREATE INDEX IF NOT EXISTS idx_devices_active ON devices(number,active)`,
	}
	for _, q := range stmts {
		if err := s.exec(q); err != nil {
			s.Close()
			return nil, err
		}
	}
	// In-place migrations from v0.1.0. Existing devices remain manual.
	migrations := []struct{ name, ddl string }{
		{"source", `ALTER TABLE devices ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'`},
		{"last_seen", `ALTER TABLE devices ADD COLUMN last_seen TEXT NOT NULL DEFAULT ''`},
		{"contact_hash", `ALTER TABLE devices ADD COLUMN contact_hash TEXT NOT NULL DEFAULT ''`},
	}
	for _, m := range migrations {
		exists, err := s.hasColumn("devices", m.name)
		if err != nil {
			s.Close()
			return nil, err
		}
		if !exists {
			if err := s.exec(m.ddl); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *Store) hasColumn(table, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.prepare(`SELECT count(*) FROM pragma_table_info('devices') WHERE name=?`, name)
	if err != nil {
		return false, err
	}
	defer C.sqlite3_finalize(st)
	if C.sqlite3_step(st) != C.SQLITE_ROW {
		return false, s.sqliteErr()
	}
	return C.sqlite3_column_int(st, 0) > 0, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	rc := C.sqlite3_close(s.db)
	if rc != C.SQLITE_OK {
		return errors.New("sqlite close failed")
	}
	s.db = nil
	return nil
}
func (s *Store) sqliteErr() error { return errors.New(C.GoString(C.sqlite3_errmsg(s.db))) }
func (s *Store) prepare(q string, args ...string) (*C.sqlite3_stmt, error) {
	cq := C.CString(q)
	defer C.free(unsafe.Pointer(cq))
	var stmt *C.sqlite3_stmt
	if C.sqlite3_prepare_v2(s.db, cq, -1, &stmt, nil) != C.SQLITE_OK {
		return nil, s.sqliteErr()
	}
	for i, a := range args {
		ca := C.CString(a)
		rc := C.sqlite3_bind_text(stmt, C.int(i+1), ca, C.int(len(a)), C.SQLITE_TRANSIENT)
		C.free(unsafe.Pointer(ca))
		if rc != C.SQLITE_OK {
			C.sqlite3_finalize(stmt)
			return nil, s.sqliteErr()
		}
	}
	return stmt, nil
}
func (s *Store) exec(q string, args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execLocked(q, args...)
}
func (s *Store) execLocked(q string, args ...string) error {
	stmt, err := s.prepare(q, args...)
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(stmt)
	for {
		rc := C.sqlite3_step(stmt)
		if rc == C.SQLITE_DONE {
			return nil
		}
		if rc != C.SQLITE_ROW {
			return s.sqliteErr()
		}
	}
}
func col(stmt *C.sqlite3_stmt, i int) string {
	p := C.sqlite3_column_text(stmt, C.int(i))
	if p == nil {
		return ""
	}
	return C.GoString((*C.char)(unsafe.Pointer(p)))
}
func now() string { return time.Now().UTC().Format(time.RFC3339) }
func suffix(token string) string {
	if len(token) <= 8 {
		return "********"
	}
	return "…" + token[len(token)-8:]
}

func (s *Store) SetEnabled(number string, enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return s.exec(`INSERT INTO extensions(number,enabled,updated_at) VALUES(?,?,?)
        ON CONFLICT(number) DO UPDATE SET enabled=excluded.enabled, updated_at=excluded.updated_at`, number, v, now())
}
func (s *Store) UpsertDevice(number, id, token, param string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.execLocked(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.execLocked(`ROLLBACK`)
		}
	}()
	ts := now()
	if err := s.execLocked(`INSERT OR IGNORE INTO extensions(number,enabled,updated_at) VALUES(?,0,?)`, number, ts); err != nil {
		return err
	}
	if err := s.execLocked(`INSERT INTO devices(number,device_id,token,pn_param,active,updated_at,last_error)
        VALUES(?,?,?,?,1,?,'') ON CONFLICT(number,device_id) DO UPDATE SET
        token=excluded.token, pn_param=excluded.pn_param,active=1,source='manual',
        updated_at=excluded.updated_at,last_error=''`, number, id, token, param, ts); err != nil {
		return err
	}
	if err := s.execLocked(`COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}
func (s *Store) RemoveDevice(number, id string) error {
	return s.exec(`DELETE FROM devices WHERE number=? AND device_id=?`, number, id)
}
func (s *Store) DisableDevice(number, id, reason string) error {
	return s.exec(`UPDATE devices SET active=0,last_error=?,updated_at=? WHERE number=? AND device_id=?`, reason, now(), number, id)
}
func (s *Store) ListExtensions() ([]Extension, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.prepare(`SELECT e.number,e.enabled,e.updated_at,
        (SELECT count(*) FROM devices d WHERE d.number=e.number AND d.active=1)
        FROM extensions e ORDER BY e.number`)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(st)
	out := []Extension{}
	for {
		rc := C.sqlite3_step(st)
		if rc == C.SQLITE_DONE {
			return out, nil
		}
		if rc != C.SQLITE_ROW {
			return nil, s.sqliteErr()
		}
		out = append(out, Extension{Number: col(st, 0), PushEnabled: C.sqlite3_column_int(st, 1) != 0, UpdatedAt: col(st, 2), Devices: int(C.sqlite3_column_int(st, 3))})
	}
}
func (s *Store) GetExtension(number string) (Extension, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.prepare(`SELECT e.number,e.enabled,e.updated_at,
        (SELECT count(*) FROM devices d WHERE d.number=e.number AND d.active=1)
        FROM extensions e WHERE e.number=?`, number)
	if err != nil {
		return Extension{}, false, err
	}
	defer C.sqlite3_finalize(st)
	rc := C.sqlite3_step(st)
	if rc == C.SQLITE_DONE {
		return Extension{}, false, nil
	}
	if rc != C.SQLITE_ROW {
		return Extension{}, false, s.sqliteErr()
	}
	return Extension{Number: col(st, 0), PushEnabled: C.sqlite3_column_int(st, 1) != 0, UpdatedAt: col(st, 2), Devices: int(C.sqlite3_column_int(st, 3))}, true, nil
}
func (s *Store) ListDevices(number string, onlyActive bool) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sql := `SELECT device_id,token,pn_param,active,updated_at,last_error,source,last_seen FROM devices WHERE number=?`
	if onlyActive {
		sql += ` AND active=1`
	}
	sql += ` ORDER BY device_id`
	st, err := s.prepare(sql, number)
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(st)
	out := []Device{}
	for {
		rc := C.sqlite3_step(st)
		if rc == C.SQLITE_DONE {
			return out, nil
		}
		if rc != C.SQLITE_ROW {
			return nil, s.sqliteErr()
		}
		token := col(st, 1)
		out = append(out, Device{ID: col(st, 0), Token: token, TokenSuffix: suffix(token), PNParam: col(st, 2), Active: C.sqlite3_column_int(st, 3) != 0, UpdatedAt: col(st, 4), LastError: col(st, 5), Source: col(st, 6), LastSeen: col(st, 7)})
	}
}

// ApplyAMISnapshot writes a fully-received ContactList batch atomically.
// Missing registrations NEVER delete/disable tokens, which are needed to wake sleeping apps.
// Previous FCM-invalid tokens are not revived by subsequent identical REGISTERs.
// For explicitly configured single-contact AORs, a newly observed FCM registration
// supersedes older automatic tokens (but never the manual emergency token).
func (s *Store) ApplyAMISnapshot(contacts []AutoContact, singleContactAORs map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.execLocked(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = s.execLocked(`ROLLBACK`)
		}
	}()
	ts := now()
	current := make(map[string]map[string]bool)
	for _, c := range contacts {
		if !strings.HasPrefix(c.ID, "auto-") || c.Number == "" || c.Token == "" {
			return errors.New("invalid automatic contact")
		}
		if err := s.execLocked(`INSERT OR IGNORE INTO extensions(number,enabled,updated_at) VALUES(?,0,?)`, c.Number, ts); err != nil {
			return err
		}
		if err := s.execLocked(`INSERT INTO devices(number,device_id,token,pn_param,active,updated_at,last_error,source,last_seen,contact_hash)
            VALUES(?,?,?,?,1,?,'','asterisk',?,?)
            ON CONFLICT(number,device_id) DO UPDATE SET
              active=CASE WHEN devices.token<>excluded.token THEN 1 ELSE devices.active END,
              last_error=CASE WHEN devices.token<>excluded.token THEN '' ELSE devices.last_error END,
              token=excluded.token,pn_param=excluded.pn_param,updated_at=excluded.updated_at,
              source='asterisk',last_seen=excluded.last_seen,contact_hash=excluded.contact_hash`,
			c.Number, c.ID, c.Token, c.PNParam, ts, ts, c.ContactHash); err != nil {
			return err
		}
		if current[c.Number] == nil {
			current[c.Number] = make(map[string]bool)
		}
		current[c.Number][c.ID] = true
	}
	for number, ids := range current {
		if !singleContactAORs[number] {
			continue
		}
		// This policy is only safe when the operator configured max_contacts=1.
		for activeID := range ids {
			if err := s.execLocked(`UPDATE devices SET active=0,last_error='superseded_by_registration'
                WHERE number=? AND source='asterisk' AND device_id<>? AND active=1`, number, activeID); err != nil {
				return err
			}
		}
	}
	if err := s.execLocked(`COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// MetricCounts aggregates device state without revealing numbers or tokens.
type MetricCounts struct {
	AutoActive, AutoInactive, ManualActive, ManualInactive int64
	EnabledExtensions, DisabledExtensions                  int64
}

func (s *Store) MetricsCounts() (MetricCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := MetricCounts{}
	st, err := s.prepare(`SELECT
    sum(CASE WHEN source='asterisk' AND active=1 THEN 1 ELSE 0 END),
    sum(CASE WHEN source='asterisk' AND active=0 THEN 1 ELSE 0 END),
    sum(CASE WHEN source='manual' AND active=1 THEN 1 ELSE 0 END),
    sum(CASE WHEN source='manual' AND active=0 THEN 1 ELSE 0 END)
    FROM devices`)
	if err != nil {
		return result, err
	}
	if C.sqlite3_step(st) != C.SQLITE_ROW {
		C.sqlite3_finalize(st)
		return result, s.sqliteErr()
	}
	result.AutoActive = int64(C.sqlite3_column_int64(st, 0))
	result.AutoInactive = int64(C.sqlite3_column_int64(st, 1))
	result.ManualActive = int64(C.sqlite3_column_int64(st, 2))
	result.ManualInactive = int64(C.sqlite3_column_int64(st, 3))
	C.sqlite3_finalize(st)
	st, err = s.prepare(`SELECT sum(CASE WHEN enabled=1 THEN 1 ELSE 0 END),sum(CASE WHEN enabled=0 THEN 1 ELSE 0 END) FROM extensions`)
	if err != nil {
		return result, err
	}
	defer C.sqlite3_finalize(st)
	if C.sqlite3_step(st) != C.SQLITE_ROW {
		return result, s.sqliteErr()
	}
	result.EnabledExtensions = int64(C.sqlite3_column_int64(st, 0))
	result.DisabledExtensions = int64(C.sqlite3_column_int64(st, 1))
	return result, nil
}
