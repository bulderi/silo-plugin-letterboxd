package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

// A watchlist traversal reads every Letterboxd page before it hands any film
// to the host, and then serves the films from memory over further
// ListRemoteState pages. Reading the whole list first means a film removed on
// Letterboxd while the host is paging cannot shift the list and make another
// film look removed. Both the page reads and the film lookups may spread over
// several host pages.
//
// A snapshot is used by one traversal at a time: the host requests its pages
// in order.
type snapshot struct {
	id       string
	username string
	reader   *letterboxd.WatchlistReader
	// films is set once every watchlist page is read.
	films []letterboxd.Poster
	// offset is the next film to hand out.
	offset int
	// seq is the page number the next token must carry, so a replayed or
	// stale token is refused.
	seq     int
	touched time.Time
}

// pagesDone reports whether the watchlist pages are all read.
func (s *snapshot) pagesDone() bool { return s.films != nil || s.reader.Done() }

// finished reports whether every film has been handed out.
func (s *snapshot) finished() bool { return s.pagesDone() && s.offset >= len(s.films) }

type snapshotStore struct {
	// ttl is how long an unused snapshot is kept.
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	items map[string]*snapshot
}

func newSnapshotStore(ttl time.Duration, now func() time.Time) *snapshotStore {
	return &snapshotStore{ttl: ttl, now: now, items: map[string]*snapshot{}}
}

func (s *snapshotStore) start(username string) (*snapshot, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	snap := &snapshot{
		id:       hex.EncodeToString(raw[:]),
		username: username,
		reader:   letterboxd.NewWatchlistReader(username),
		touched:  s.now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.items[snap.id] = snap
	return snap, nil
}

// resume returns the snapshot a page token names, only to the account that
// started it and only for the token issued last.
func (s *snapshotStore) resume(token, username string) (*snapshot, bool) {
	id, seq, err := parsePageToken(token)
	if err != nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	snap, ok := s.items[id]
	if !ok || !strings.EqualFold(snap.username, username) || snap.seq != seq {
		return nil, false
	}
	snap.touched = s.now()
	return snap, true
}

// nextToken issues the token for the snapshot's next page.
func (s *snapshotStore) nextToken(snap *snapshot) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap.seq++
	snap.touched = s.now()
	return pageToken(snap.id, snap.seq)
}

func (s *snapshotStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
}

func (s *snapshotStore) gcLocked() {
	now := s.now()
	for id, snap := range s.items {
		if now.Sub(snap.touched) > s.ttl {
			delete(s.items, id)
		}
	}
}

func pageToken(id string, seq int) string { return id + ":" + strconv.Itoa(seq) }

func parsePageToken(token string) (id string, seq int, err error) {
	id, rawSeq, ok := strings.Cut(token, ":")
	if !ok || id == "" {
		return "", 0, fmt.Errorf("malformed page token")
	}
	seq, err = strconv.Atoi(rawSeq)
	if err != nil || seq < 1 {
		return "", 0, fmt.Errorf("malformed page token")
	}
	return id, seq, nil
}
