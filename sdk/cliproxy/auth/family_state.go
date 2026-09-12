package auth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

const maxFamilyStateBytes = 32 << 20

type familyDiskState struct {
	Schema    int                         `json:"schema"`
	Sequence  uint64                      `json:"sequence"`
	Cursor    uint64                      `json:"cursor"`
	Families  map[string]*familyEntry     `json:"families"`
	Members   map[string]familyMember     `json:"members"`
	Exhausted map[string]familyExhaustion `json:"weekly_exhaustion"`
}

type familyStateStore struct {
	path string
	lock *os.File
}

func openFamilyState(path string) (*familyStateStore, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("family state requires an absolute file path")
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("family state directory unavailable")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("family state lock unavailable")
	}
	if errLock := lockFamilyState(lock); errLock != nil {
		_ = lock.Close()
		return nil, errors.New("family state already owned or lock unavailable")
	}
	return &familyStateStore{path: path, lock: lock}, nil
}

func (s *familyStateStore) close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func validFamilyHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(value) == 64 && len(decoded) == 32
}

// Reject duplicate object keys at every depth before decoding authoritative
// membership. A checksum cannot disambiguate a syntactically valid duplicate.
func validateFamilyJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 8 {
			return errors.New("family state nesting limit exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, isDelim := token.(json.Delim)
		if !isDelim {
			return nil
		}
		if delim != '{' && delim != '[' {
			return errors.New("unexpected family state delimiter")
		}
		seen := map[string]bool{}
		for decoder.More() {
			if delim == '{' {
				key, errKey := decoder.Token()
				name, ok := key.(string)
				if errKey != nil || !ok || seen[name] {
					return errors.New("duplicate or invalid family state key")
				}
				seen[name] = true
			}
			if errValue := value(depth + 1); errValue != nil {
				return errValue
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing family state content")
	}
	return nil
}

func (r *familyRouter) loadState() error {
	file, err := os.Open(r.store.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("family state unavailable; preserved")
	}
	defer func() { _ = file.Close() }()
	info, errInfo := file.Stat()
	if errInfo != nil || !info.Mode().IsRegular() || info.Size() > maxFamilyStateBytes {
		return errors.New("invalid family state file; preserved")
	}
	data, errRead := io.ReadAll(io.LimitReader(file, maxFamilyStateBytes+1))
	if errRead != nil || len(data) > maxFamilyStateBytes || validateFamilyJSON(data) != nil {
		return errors.New("invalid family state encoding; preserved")
	}
	var state familyDiskState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(&state); errDecode != nil || state.Schema != 1 || state.Sequence == 0 ||
		len(state.Families) > r.cfg.MaxFamilies || len(state.Members) > r.cfg.MaxMembers || len(state.Exhausted) > 128 ||
		state.Families == nil || state.Members == nil || state.Exhausted == nil {
		return errors.New("invalid family state schema or capacity; preserved")
	}
	now := r.clock()
	for key, entry := range state.Families {
		if !validFamilyHash(key) || entry == nil || !validFamilyHash(entry.Root) || entry.Generation == 0 ||
			entry.LastSeen.IsZero() || entry.LastSeen.After(now.Add(5*time.Minute)) ||
			(entry.AuthIndex == "") != (entry.Identity == "") ||
			entry.Identity != "" && (!validFamilyHash(entry.Identity) || !validFamilyIdentifier(entry.AuthIndex)) {
			return errors.New("invalid family assignment; preserved")
		}
		root, exists := state.Members[entry.Root]
		if !exists || root.Family != key || root.Parent != "" || !root.ParentKnown {
			return errors.New("missing family root membership; preserved")
		}
	}
	for key, member := range state.Members {
		if !validFamilyHash(key) || state.Families[member.Family] == nil ||
			member.Parent != "" && (!validFamilyHash(member.Parent) || !member.ParentKnown || state.Members[member.Parent].Family != member.Family) {
			return errors.New("invalid family membership; preserved")
		}
	}
	// Each vertex is visited at most twice, including very deep cold ancestry.
	colors := make(map[string]uint8, len(state.Members))
	for key := range state.Members {
		if colors[key] != 0 {
			continue
		}
		path := []string{}
		for next := key; next != ""; next = state.Members[next].Parent {
			if colors[next] == 1 {
				return errors.New("cyclic family membership; preserved")
			}
			if colors[next] == 2 {
				break
			}
			colors[next] = 1
			path = append(path, next)
		}
		for _, visited := range path {
			colors[visited] = 2
		}
	}
	for key, evidence := range state.Exhausted {
		if !validFamilyHash(key) || evidence.ObservedAt.IsZero() || evidence.ObservedAt.After(now.Add(5*time.Minute)) ||
			!evidence.ResetAt.After(evidence.ObservedAt) || evidence.ResetAt.Sub(evidence.ObservedAt) > 10*24*time.Hour {
			return errors.New("invalid weekly exhaustion evidence; preserved")
		}
	}
	r.families, r.members, r.exhausted = state.Families, state.Members, state.Exhausted
	r.sequence, r.durable, r.cursor = state.Sequence, state.Sequence, state.Cursor
	return nil
}

func (r *familyRouter) markDirtyLocked() {
	r.sequence++
	r.signalLocked()
	select {
	case r.writePending <- struct{}{}:
	default:
	}
}

func (r *familyRouter) signalLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *familyRouter) stateSnapshotLocked() familyDiskState {
	state := familyDiskState{Schema: 1, Sequence: r.sequence, Cursor: r.cursor,
		Families: make(map[string]*familyEntry, len(r.families)), Members: make(map[string]familyMember, len(r.members)),
		Exhausted: make(map[string]familyExhaustion, len(r.exhausted))}
	for key, entry := range r.families {
		copy := *entry
		state.Families[key] = &copy
	}
	for key, member := range r.members {
		state.Members[key] = member
	}
	for key, evidence := range r.exhausted {
		state.Exhausted[key] = evidence
	}
	return state
}

func (s *familyStateStore) write(state familyDiskState) error {
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxFamilyStateBytes {
		return errors.New("family state exceeds encoding limit")
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".family-write-*.tmp")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if errWrite := file.Chmod(0600); errWrite != nil {
		_ = file.Close()
		return errWrite
	}
	if _, errWrite := file.Write(data); errWrite != nil {
		_ = file.Close()
		return errWrite
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return errSync
	}
	if errClose := file.Close(); errClose != nil {
		return errClose
	}
	return replaceFamilyState(path, s.path)
}

func (r *familyRouter) flushState() {
	r.mu.Lock()
	if r.stateErr != nil || r.durable >= r.sequence {
		r.mu.Unlock()
		return
	}
	state := r.stateSnapshotLocked()
	r.mu.Unlock()
	err := r.store.write(state)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.stateErr = errors.New("family state persistence failed; no further upstream attempts permitted")
	} else {
		r.durable = state.Sequence
	}
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *familyRouter) runWriter() {
	defer close(r.writerDone)
	defer func() { _ = r.store.close() }()
	for {
		select {
		case <-r.stopWriter:
			r.flushState()
			return
		case <-r.writePending:
			r.flushState()
		}
	}
}

func (r *familyRouter) awaitDurable(ctx context.Context, sequence uint64) error {
	for {
		r.mu.Lock()
		if r.stateErr != nil || r.closed {
			err := r.unavailableLocked()
			r.mu.Unlock()
			return err
		}
		if r.durable >= sequence {
			r.mu.Unlock()
			return nil
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (r *familyRouter) stop() {
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		for key := range r.families {
			r.cancelFamilyWaitersLocked(key, familyRequestError("family_router_stopped", "family routing is stopping", 503))
		}
		close(r.changed)
		r.changed = make(chan struct{})
		close(r.stopWriter)
		r.mu.Unlock()
		<-r.writerDone
	})
}
