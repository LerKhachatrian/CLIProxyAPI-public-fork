package codexmonitor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxEntryBytes = 128 << 10

var cacheKey = regexp.MustCompile(`^[a-f0-9]{64}$`)
var orphanWrite = regexp.MustCompile(`^\.write-[0-9]+\.tmp$`)

type control struct {
	Schema         int                   `json:"schema"`
	NextStart      time.Time             `json:"next_start"`
	Starts         []time.Time           `json:"starts"`
	AutomaticReset automaticResetControl `json:"-"` // separate file preserves strict v1-reader rollback
}

// The existing store lock also owns this small companion. Keeping it beside the
// legacy cache lets an older binary ignore it without deleting new safety state.
type automaticResetControl struct {
	Schema    int       `json:"schema"`
	LastStart time.Time `json:"last_start"`
	NextStart time.Time `json:"next_start"`
}

func (c automaticResetControl) valid() bool {
	delay := c.NextStart.Sub(c.LastStart)
	return c.Schema == 1 && !c.LastStart.IsZero() && delay >= time.Minute && delay <= 2*time.Minute
}

type Store interface {
	load() (control, map[string]*Entry, error)
	saveControl(control) error
	saveEntry(*Entry) error
	removeEntry(string) error
	Close() error
}

// FileStore holds one OS lock for the coordinator lifetime. Process death
// releases it; the mere existence of the lock file is never stale ownership.
type FileStore struct {
	dir                 string
	lock                *os.File
	savedAutomaticReset automaticResetControl
}

func OpenStore(dir string) (*FileStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("monitor cache requires an absolute directory")
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("monitor cache directory unavailable")
	}
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("monitor cache lock unavailable")
	}
	if errLock := lockFile(f); errLock != nil {
		_ = f.Close()
		return nil, errors.New("monitor cache already owned or lock unavailable")
	}
	return &FileStore{dir: dir, lock: f}, nil
}

func (s *FileStore) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func readJSON(path string, capBytes int64, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, errStat := f.Stat()
	if errStat != nil || !info.Mode().IsRegular() || info.Size() > capBytes {
		return errors.New("invalid cache file")
	}
	data, errRead := io.ReadAll(io.LimitReader(f, capBytes+1))
	if errRead != nil || int64(len(data)) > capBytes {
		return errors.New("cache read failed")
	}
	if _, errJSON := strictObject(data); errJSON != nil {
		return errJSON
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

func (s *FileStore) load() (control, map[string]*Entry, error) {
	c := control{Schema: 1}
	entries := make(map[string]*Entry)
	err := readJSON(filepath.Join(s.dir, "control.json"), 4096, &c)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, nil, errors.New("monitor control cache invalid; preserved")
	}
	if c.Schema != 1 || len(c.Starts) > 6 {
		return c, nil, errors.New("unsupported monitor control cache; preserved")
	}
	err = readJSON(s.automaticResetPath(), 4096, &c.AutomaticReset)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !c.AutomaticReset.valid() {
		return c, nil, errors.New("automatic reset pacing cache invalid; preserved")
	}
	s.savedAutomaticReset = c.AutomaticReset
	directory, err := os.Open(s.dir)
	if err != nil {
		return c, nil, errors.New("monitor cache unavailable")
	}
	defer func() { _ = directory.Close() }()
	files, err := directory.ReadDir(MaxAccounts + 5)
	if err != nil && err != io.EOF {
		return c, nil, errors.New("monitor cache listing failed")
	}
	if len(files) > MaxAccounts+3 {
		return c, nil, errors.New("monitor cache file limit exceeded; preserved")
	}
	grants := 0
	for _, file := range files {
		name := file.Name()
		if name == "control.json" || name == "owner.lock" {
			continue
		}
		if orphanWrite.MatchString(name) && file.Type().IsRegular() {
			// An exclusive lifetime lock proves no writer still owns this exact
			// generated temporary file. A crash before rename left the prior
			// complete entry intact; discard only this regenerable partial write.
			if errRemove := os.Remove(filepath.Join(s.dir, name)); errRemove != nil {
				return c, nil, errors.New("interrupted cache write cleanup failed")
			}
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if file.IsDir() || !strings.HasSuffix(name, ".json") || !cacheKey.MatchString(key) {
			return c, nil, errors.New("unexpected monitor cache file; preserved")
		}
		var entry Entry
		if errRead := readJSON(filepath.Join(s.dir, name), maxEntryBytes, &entry); errRead != nil || !validEntry(&entry, key) {
			return c, nil, errors.New("monitor account cache invalid; preserved")
		}
		if entry.Bank != nil {
			grants += len(entry.Bank.Credits)
		}
		entries[key] = &entry
	}
	if grants > MaxGrants || len(entries) > MaxAccounts {
		return c, nil, errors.New("monitor cache capacity exceeded; preserved")
	}
	return c, entries, nil
}

func validEntry(e *Entry, key string) bool {
	if e.Schema != 1 || e.Key != key || !cacheKey.MatchString(key) {
		return false
	}
	for _, lane := range []Lane{e.UsageSchedule, e.ResetSchedule} {
		if lane.Failures < 0 || lane.Failures > 10 || !validLaneError(lane.Error) {
			return false
		}
	}
	if e.Usage != nil {
		u := e.Usage
		if u.ObservedAt.IsZero() || !useful(u) || !uniqueMainWindows(u) || len(u.Windows) > 18 || (u.Source != "passive" && u.Source != "provider_check") || u.Plan != plan(u.Plan) {
			return false
		}
		for _, w := range u.Windows {
			normalized := window(w.UsedPercent, float64(w.DurationSeconds), &w.ResetAt, w.Kind == "additional", "Quota")
			if normalized == nil || normalized.Kind != w.Kind || normalized.Label != w.Label {
				return false
			}
			switch w.Kind {
			case "weekly", "five_hour", "monthly", "additional", "unknown":
			default:
				return false
			}
			switch w.Label {
			case "Weekly", "5 hour", "Monthly", "Additional quota", "Quota":
			default:
				return false
			}
		}
	}
	if e.Bank != nil {
		if e.Bank.ObservedAt.IsZero() || len(e.Bank.Credits) > MaxAccountGrants {
			return false
		}
		data, err := json.Marshal(e.Bank)
		if err != nil {
			return false
		}
		if parsed, errBank := ParseBank(data, e.Bank.ObservedAt); errBank != nil || e.Bank.Complete && !parsed.Complete {
			return false
		}
	}
	return true
}

func validLaneError(s string) bool {
	switch s {
	case "", "oauth_rejected", "provider_blocked", "rate_limited", "provider_unavailable", "invalid_observation", "persistence_unavailable", "interrupted_check", "refresh_expired", "action_changed", "observation_needed":
		return true
	}
	return false
}

func (s *FileStore) saveControl(c control) error {
	if c.AutomaticReset != s.savedAutomaticReset {
		if !c.AutomaticReset.valid() || c.AutomaticReset.LastStart.Before(s.savedAutomaticReset.LastStart) {
			return errors.New("invalid automatic reset pacing deadline")
		}
		// Advance the independent floor before recording the common budget. A
		// failure of either write prevents dispatch; it can lose an opportunity
		// to check, never permit an unrecorded provider attempt after a crash.
		if err := s.writeFile(s.automaticResetPath(), c.AutomaticReset, 4096); err != nil {
			return err
		}
		s.savedAutomaticReset = c.AutomaticReset
	}
	return s.write("control.json", c, 4096)
}

func (s *FileStore) automaticResetPath() string {
	return s.dir + ".automatic-resets.v1.json"
}

func (s *FileStore) saveEntry(e *Entry) error {
	if !validEntry(e, e.Key) {
		return errors.New("invalid monitor cache entry")
	}
	return s.write(e.Key+".json", e, maxEntryBytes)
}

func (s *FileStore) removeEntry(key string) error {
	if !cacheKey.MatchString(key) {
		return errors.New("invalid monitor cache key")
	}
	err := os.Remove(filepath.Join(s.dir, key+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *FileStore) write(name string, value any, capBytes int) error {
	return s.writeFile(filepath.Join(s.dir, name), value, capBytes)
}

func (s *FileStore) writeFile(path string, value any, capBytes int) error {
	data, err := json.Marshal(value)
	if err != nil || len(data) > capBytes {
		return errors.New("monitor cache serialization failed")
	}
	tmp, err := os.CreateTemp(s.dir, ".write-*.tmp")
	if err != nil {
		return errors.New("monitor cache write unavailable")
	}
	tmpName := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(tmpName) }()
	if _, errWrite := tmp.Write(data); errWrite != nil {
		return errors.New("monitor cache write failed")
	}
	if errSync := tmp.Sync(); errSync != nil {
		return errors.New("monitor cache sync failed")
	}
	if errClose := tmp.Close(); errClose != nil {
		return errors.New("monitor cache close failed")
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		return errors.New("monitor cache replacement failed")
	}
	return nil
}
