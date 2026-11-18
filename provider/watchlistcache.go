package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

// A watchlist read must be a complete snapshot, which takes every page, but
// most syncs find the watchlist unchanged. Letterboxd lists the watchlist
// newest first: an added film appears on the first page and a removed one
// lowers the entry count. So when the first page shows the same films and
// the same count as the last full read, the list has not changed, and the
// plugin serves that read instead of loading the remaining pages. A full
// read still runs at least every watchlistFullEvery.
const (
	watchlistKeyPrefix = "watchlist/"
	watchlistFullEvery = 24 * time.Hour
)

// watchlistSnapshot is the last full watchlist read of one account.
type watchlistSnapshot struct {
	// Total is the entry count the first page reported.
	Total int `json:"t"`
	// First is how many films the first page held.
	First int `json:"f"`
	// FullAt is when the read finished, in Unix seconds.
	FullAt int64          `json:"a"`
	Films  []cachedPoster `json:"p"`
}

type cachedPoster struct {
	LID  string `json:"l"`
	Slug string `json:"s"`
	Name string `json:"n,omitempty"`
}

// watchlistStore keeps each account's last full watchlist read in instance
// state, one key per account, and in memory once read.
type watchlistStore struct {
	store StateStore
	now   func() time.Time

	mu        sync.Mutex
	snapshots map[string]watchlistSnapshot
}

func newWatchlistStore(store StateStore, now func() time.Time) *watchlistStore {
	return &watchlistStore{store: store, now: now, snapshots: map[string]watchlistSnapshot{}}
}

// unchanged returns the last full read when the first page and entry count
// match it and it is recent enough. Anything it cannot read means a full
// read.
func (w *watchlistStore) unchanged(ctx context.Context, username string, firstPage []letterboxd.Poster, total int) ([]letterboxd.Poster, bool) {
	if total < 0 || len(firstPage) == 0 {
		return nil, false
	}
	snap, ok := w.get(ctx, username)
	if !ok || snap.Total != total || snap.First != len(firstPage) || len(snap.Films) != total ||
		w.now().Sub(time.Unix(snap.FullAt, 0)) >= watchlistFullEvery {
		return nil, false
	}
	if !sameLIDs(firstPage, snap.Films[:len(firstPage)]) {
		return nil, false
	}
	films := make([]letterboxd.Poster, len(snap.Films))
	for i, p := range snap.Films {
		films[i] = letterboxd.Poster{LID: p.LID, Slug: p.Slug, Name: p.Name}
	}
	return films, true
}

// save records a full read. A write the host refuses, such as one over its
// size limit for a very long watchlist, only means full reads continue.
func (w *watchlistStore) save(ctx context.Context, username string, films []letterboxd.Poster, firstPage, total int) error {
	snap := watchlistSnapshot{Total: total, First: firstPage, FullAt: w.now().Unix()}
	for _, f := range films {
		snap.Films = append(snap.Films, cachedPoster{LID: f.LID, Slug: f.Slug, Name: f.Name})
	}
	key := strings.ToLower(username)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.store != nil {
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if err := w.store.Write(ctx, watchlistKeyPrefix+key, raw); err != nil {
			delete(w.snapshots, key)
			return fmt.Errorf("write watchlist snapshot: %w", err)
		}
	}
	w.snapshots[key] = snap
	return nil
}

func (w *watchlistStore) get(ctx context.Context, username string) (watchlistSnapshot, bool) {
	key := strings.ToLower(username)
	w.mu.Lock()
	defer w.mu.Unlock()
	if snap, ok := w.snapshots[key]; ok {
		return snap, true
	}
	if w.store == nil {
		return watchlistSnapshot{}, false
	}
	raw, ok, err := w.store.Read(ctx, watchlistKeyPrefix+key)
	if err != nil || !ok {
		return watchlistSnapshot{}, false
	}
	var snap watchlistSnapshot
	if json.Unmarshal(raw, &snap) != nil {
		return watchlistSnapshot{}, false
	}
	w.snapshots[key] = snap
	return snap, true
}

// sameLIDs reports whether two film lists hold the same films in order.
func sameLIDs(a []letterboxd.Poster, b []cachedPoster) bool {
	return slices.EqualFunc(a, b, func(p letterboxd.Poster, c cachedPoster) bool { return p.LID == c.LID })
}
