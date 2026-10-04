package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

// StateStore is the host's per-installation key/value store (runtime host
// instance state). Keys are at most 256 bytes, values at most 256 KiB, and an
// installation holds at most 256 keys.
type StateStore interface {
	Read(ctx context.Context, key string) ([]byte, bool, error)
	Write(ctx context.Context, key string, value []byte) error
}

const (
	cacheShards        = 64
	maxShardBytes      = 240 << 10 // headroom under the host's 256 KiB value limit
	positiveTTL        = 180 * 24 * time.Hour
	negativeTTL        = 30 * 24 * time.Hour
	cacheShardKeyFmt   = "films/%02x"
	cacheSchemaVersion = 1
)

// cachedFilm is a resolved film page. Films without a TMDB movie link are
// cached too, so TV entries and unlinked shorts are not refetched every sync.
type cachedFilm struct {
	LID       string `json:"l"`
	Slug      string `json:"s"`
	Title     string `json:"t,omitempty"`
	Year      int    `json:"y,omitempty"`
	TMDBID    string `json:"tm,omitempty"`
	TMDBType  string `json:"tt,omitempty"`
	IMDbID    string `json:"im,omitempty"`
	CheckedAt int64  `json:"c"`
}

func cachedFromPage(f letterboxd.FilmPage, now time.Time) cachedFilm {
	return cachedFilm{
		LID: f.LID, Slug: f.Slug, Title: f.Title, Year: f.Year,
		TMDBID: f.TMDBID, TMDBType: f.TMDBType, IMDbID: f.IMDbID,
		CheckedAt: now.Unix(),
	}
}

func (f cachedFilm) isMovie() bool {
	return f.TMDBType == "movie" && (f.TMDBID != "" || f.IMDbID != "")
}

func (f cachedFilm) fresh(now time.Time) bool {
	ttl := negativeTTL
	if f.isMovie() {
		ttl = positiveTTL
	}
	return now.Sub(time.Unix(f.CheckedAt, 0)) < ttl
}

type shardFile struct {
	Version int                   `json:"v"`
	Films   map[string]cachedFilm `json:"f"`
}

// filmCache maps Letterboxd films to their external ids. It is loaded from the
// host on first use and written back shard by shard, so a sync that stops
// halfway keeps every lookup it already paid for.
type filmCache struct {
	store StateStore
	now   func() time.Time

	mu     sync.Mutex
	loaded bool
	byLID  map[string]cachedFilm
	byTMDB map[string]string
	byIMDb map[string]string
	dirty  map[int]bool
}

func newFilmCache(store StateStore, now func() time.Time) *filmCache {
	return &filmCache{
		store:  store,
		now:    now,
		byLID:  map[string]cachedFilm{},
		byTMDB: map[string]string{},
		byIMDb: map[string]string{},
		dirty:  map[int]bool{},
	}
}

func shardOf(lid string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(lid))
	return int(h.Sum32() % cacheShards)
}

// load reads every shard once. A store failure leaves the cache unloaded:
// lookups made meanwhile stay in memory, and flush waits until a later load
// succeeds, so a partial cache never overwrites the stored shards.
func (c *filmCache) load(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded || c.store == nil {
		c.loaded = true
		return nil
	}
	for shard := 0; shard < cacheShards; shard++ {
		raw, found, err := c.store.Read(ctx, fmt.Sprintf(cacheShardKeyFmt, shard))
		if err != nil {
			return fmt.Errorf("read film cache: %w", err)
		}
		if !found {
			continue
		}
		var file shardFile
		if json.Unmarshal(raw, &file) != nil || file.Version != cacheSchemaVersion {
			continue // unreadable or old shard: refetching is cheap enough
		}
		for lid, film := range file.Films {
			if film.LID != lid || !letterboxd.ValidLID(lid) {
				continue
			}
			// A lookup made while the store was unreachable is newer.
			if current, ok := c.byLID[lid]; ok && current.CheckedAt >= film.CheckedAt {
				continue
			}
			c.indexLocked(film)
		}
	}
	c.loaded = true
	return nil
}

func (c *filmCache) indexLocked(film cachedFilm) {
	if old, ok := c.byLID[film.LID]; ok {
		if c.byTMDB[old.TMDBID] == old.LID {
			delete(c.byTMDB, old.TMDBID)
		}
		if c.byIMDb[old.IMDbID] == old.LID {
			delete(c.byIMDb, old.IMDbID)
		}
	}
	c.byLID[film.LID] = film
	if film.TMDBID != "" && film.TMDBType == "movie" {
		c.byTMDB[film.TMDBID] = film.LID
	}
	if film.IMDbID != "" {
		c.byIMDb[film.IMDbID] = film.LID
	}
}

// byLIDAny returns a cached film however old it is.
func (c *filmCache) byLIDAny(lid string) (cachedFilm, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	film, ok := c.byLID[lid]
	return film, ok
}

// byLIDFresh returns a cached film that is still fresh.
func (c *filmCache) byLIDFresh(lid string) (cachedFilm, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	film, ok := c.byLID[lid]
	if !ok || !film.fresh(c.now()) {
		return cachedFilm{}, false
	}
	return film, true
}

// byExternal looks a film up by "tmdb" or "imdb" id.
func (c *filmCache) byExternal(namespace, id string) (cachedFilm, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.byTMDB
	if namespace == "imdb" {
		index = c.byIMDb
	}
	lid, ok := index[id]
	if !ok {
		return cachedFilm{}, false
	}
	film, ok := c.byLID[lid]
	if !ok || !film.fresh(c.now()) {
		return cachedFilm{}, false
	}
	return film, true
}

func (c *filmCache) put(film cachedFilm) {
	if !letterboxd.ValidLID(film.LID) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.indexLocked(film)
	c.dirty[shardOf(film.LID)] = true
}

// flush writes the changed shards. A shard that outgrows the host's value
// limit drops its oldest entries.
func (c *filmCache) flush(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.dirty = map[int]bool{}
		return nil
	}
	if !c.loaded || len(c.dirty) == 0 {
		return nil
	}
	shards := make([]int, 0, len(c.dirty))
	for shard := range c.dirty {
		shards = append(shards, shard)
	}
	sort.Ints(shards)
	for _, shard := range shards {
		raw, err := c.encodeShardLocked(shard)
		if err != nil {
			return err
		}
		if err := c.store.Write(ctx, fmt.Sprintf(cacheShardKeyFmt, shard), raw); err != nil {
			return fmt.Errorf("write film cache: %w", err)
		}
		delete(c.dirty, shard)
	}
	return nil
}

func (c *filmCache) encodeShardLocked(shard int) ([]byte, error) {
	var films []cachedFilm
	for lid, film := range c.byLID {
		if shardOf(lid) == shard {
			films = append(films, film)
		}
	}
	// Newest first, so trimming drops the entries checked longest ago.
	sort.Slice(films, func(i, j int) bool {
		if films[i].CheckedAt != films[j].CheckedAt {
			return films[i].CheckedAt > films[j].CheckedAt
		}
		return films[i].LID < films[j].LID
	})
	for {
		file := shardFile{Version: cacheSchemaVersion, Films: make(map[string]cachedFilm, len(films))}
		for _, film := range films {
			file.Films[film.LID] = film
		}
		raw, err := json.Marshal(file)
		if err != nil {
			return nil, err
		}
		if len(raw) <= maxShardBytes || len(films) == 0 {
			return raw, nil
		}
		keep := len(films) * 9 / 10
		for _, dropped := range films[keep:] {
			delete(c.byLID, dropped.LID)
		}
		films = films[:keep]
	}
}
