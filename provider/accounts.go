package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// accountsKey is the instance-state key holding what the plugin keeps per
// Letterboxd account, outside the connection's credentials so connecting
// again with a new cookie keeps it.
const accountsKey = "accounts"

// accountState is what the plugin keeps for one Letterboxd account.
type accountState struct {
	// DiaryStart is when the plugin first saw a play to log, in Unix seconds.
	// Silo queues every play in a profile's history when watched export is
	// turned on; plays from more than diaryGrace before this are not logged,
	// so the export does not flood the diary with old entries.
	DiaryStart int64 `json:"s,omitempty"`
	// Entries are the diary entries the plugin logged in the last
	// diaryEntryWindow, so a rating given in Silo after watching can be put
	// on the entry.
	Entries []loggedEntry `json:"e,omitempty"`
}

// loggedEntry is a diary entry the plugin logged.
type loggedEntry struct {
	LID string `json:"l"` // the film
	ID  string `json:"i"` // the entry
	// Logged is when the plugin logged it, in Unix seconds.
	Logged int64 `json:"t"`
	// Updated is the entry's whenUpdated after the plugin's last write; a
	// different value means someone else changed the entry since.
	Updated string `json:"u"`
}

// maxLoggedEntries bounds the entries kept per account.
const maxLoggedEntries = 200

// accountStore keeps every account's state in memory once read, so the host
// store is read once per process, and writes the whole set on each change.
// Without a host store (tests) it keeps the state in memory only.
type accountStore struct {
	store StateStore
	now   func() time.Time

	mu       sync.Mutex
	accounts map[string]accountState
	loaded   bool
}

func newAccountStore(store StateStore, now func() time.Time) *accountStore {
	return &accountStore{store: store, now: now, accounts: map[string]accountState{}}
}

// get returns the account's state. An unreadable store is an error, never an
// empty state, so an outage cannot look like a fresh start.
func (a *accountStore) get(ctx context.Context, username string) (accountState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.loadLocked(ctx); err != nil {
		return accountState{}, err
	}
	return a.accounts[strings.ToLower(username)], nil
}

// update applies change to a copy of the account's state and keeps it only
// once it is written, so a failed write changes nothing. Entries older than
// diaryEntryWindow are dropped on every write.
func (a *accountStore) update(ctx context.Context, username string, change func(*accountState)) (accountState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.loadLocked(ctx); err != nil {
		return accountState{}, err
	}
	key := strings.ToLower(username)
	state := a.accounts[key]
	state.Entries = slices.Clone(state.Entries)
	change(&state)
	cutoff := a.now().Add(-diaryEntryWindow).Unix()
	state.Entries = slices.DeleteFunc(state.Entries, func(e loggedEntry) bool { return e.Logged < cutoff })
	if n := len(state.Entries); n > maxLoggedEntries {
		state.Entries = state.Entries[n-maxLoggedEntries:]
	}
	if a.store != nil {
		next := make(map[string]accountState, len(a.accounts)+1)
		for account, s := range a.accounts {
			next[account] = s
		}
		next[key] = state
		raw, err := json.Marshal(next)
		if err != nil {
			return accountState{}, err
		}
		if err := a.store.Write(ctx, accountsKey, raw); err != nil {
			return accountState{}, fmt.Errorf("write account state: %w", err)
		}
	}
	a.accounts[key] = state
	return state, nil
}

func (a *accountStore) loadLocked(ctx context.Context) error {
	if a.store == nil || a.loaded {
		return nil
	}
	raw, ok, err := a.store.Read(ctx, accountsKey)
	if err != nil {
		return fmt.Errorf("read account state: %w", err)
	}
	if ok {
		if err := json.Unmarshal(raw, &a.accounts); err != nil {
			return fmt.Errorf("decode account state: %w", err)
		}
	}
	a.loaded = true
	return nil
}

// diaryStart returns when the account's diary export started, recording now
// if it has not.
func (a *accountStore) diaryStart(ctx context.Context, username string) (time.Time, error) {
	state, err := a.get(ctx, username)
	if err == nil && state.DiaryStart == 0 {
		state, err = a.update(ctx, username, func(s *accountState) {
			if s.DiaryStart == 0 {
				s.DiaryStart = a.now().Unix()
			}
		})
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(state.DiaryStart, 0), nil
}

// newestEntry returns the newest entry for the film the plugin logged within
// diaryEntryWindow.
func (a *accountStore) newestEntry(ctx context.Context, username, lid string) (loggedEntry, bool, error) {
	state, err := a.get(ctx, username)
	if err != nil {
		return loggedEntry{}, false, err
	}
	cutoff := a.now().Add(-diaryEntryWindow).Unix()
	var newest loggedEntry
	found := false
	for _, e := range state.Entries {
		if e.LID == lid && e.Logged >= cutoff && (!found || e.Logged >= newest.Logged) {
			newest, found = e, true
		}
	}
	return newest, found, nil
}

func (a *accountStore) addEntry(ctx context.Context, username string, entry loggedEntry) error {
	_, err := a.update(ctx, username, func(s *accountState) { s.Entries = append(s.Entries, entry) })
	return err
}

func (a *accountStore) replaceEntry(ctx context.Context, username string, entry loggedEntry) error {
	_, err := a.update(ctx, username, func(s *accountState) {
		for i := range s.Entries {
			if s.Entries[i].ID == entry.ID {
				s.Entries[i] = entry
			}
		}
	})
	return err
}

// dropEntry forgets an entry that no longer exists. A failed write only
// leaves it to be looked up and dropped again.
func (a *accountStore) dropEntry(ctx context.Context, username, id string) {
	_, _ = a.update(ctx, username, func(s *accountState) {
		s.Entries = slices.DeleteFunc(s.Entries, func(e loggedEntry) bool { return e.ID == id })
	})
}
