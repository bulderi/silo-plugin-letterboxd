package provider

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

// Ratings and diary reads keep a cursor in Silo, so a sync reads only the
// newest pages instead of the whole list. Ratings come most recently rated
// first and the diary newest entry first, so the read stops once it reaches
// entries the last read saw. Silo saves the cursor only after it has taken
// every item of a read, and treats it as a hint: a missing or unreadable
// cursor means a full read.
//
// An incremental ratings read is not a snapshot, so it cannot show a removed
// rating; a full read on a schedule does. The diary is never a snapshot, and
// its full read picks up entries logged later for dates older than the
// incremental window.
const (
	cursorVersion = 1
	// ratingsHeadSize is how many of the most recently rated films a cursor
	// remembers. A read stops at the first page holding one of them with
	// the same rating.
	ratingsHeadSize = 20
	// fullRatingsEvery is how often a ratings read is a complete snapshot,
	// which is how a rating removed on Letterboxd reaches Silo.
	fullRatingsEvery = 24 * time.Hour
	// fullDiaryEvery is how often a diary read reads every page.
	fullDiaryEvery = 7 * 24 * time.Hour
	// diaryLookbackDays is how far before the newest entry of the last read
	// an incremental diary read goes, for entries logged late.
	diaryLookbackDays = 14
)

// readCursor is the cursor of a ratings or diary read.
type readCursor struct {
	V int `json:"v"`
	// Full is when the last full read finished, in Unix seconds.
	Full int64 `json:"full"`
	// Head is "<lid>:<rating>" for the most recently rated films (ratings).
	Head []string `json:"head,omitempty"`
	// Newest is the date of the newest diary entry, YYYY-MM-DD (diary).
	Newest string `json:"newest,omitempty"`
}

func parseCursor(raw string) (readCursor, bool) {
	var c readCursor
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &c) != nil || c.V != cursorVersion || c.Full <= 0 {
		return readCursor{}, false
	}
	return c, true
}

func (c readCursor) encode() string {
	c.V = cursorVersion
	raw, _ := json.Marshal(c)
	return string(raw)
}

func headKey(film letterboxd.Poster) string {
	return film.LID + ":" + strconv.Itoa(film.Rating)
}

// readPlan is how a ratings or diary traversal reads, decided when it starts.
type readPlan struct {
	incremental bool
	previous    readCursor
	stop        func(letterboxd.ListPage) bool
}

// planRead decides from the host's cursor whether a read can stop early.
func (p *Provider) planRead(kind letterboxd.ListKind, rawCursor string) readPlan {
	previous, ok := parseCursor(rawCursor)
	if !ok || kind == letterboxd.ListWatchlist {
		return readPlan{}
	}
	plan := readPlan{previous: previous}
	age := p.now().Sub(time.Unix(previous.Full, 0))
	switch kind {
	case letterboxd.ListRatings:
		if age >= fullRatingsEvery || len(previous.Head) == 0 {
			return plan
		}
		seen := make(map[string]bool, len(previous.Head))
		for _, key := range previous.Head {
			seen[key] = true
		}
		plan.incremental = true
		plan.stop = func(page letterboxd.ListPage) bool {
			for _, film := range page.Films {
				if seen[headKey(film)] {
					return true
				}
			}
			return false
		}
	case letterboxd.ListDiary:
		if age >= fullDiaryEvery || previous.Newest == "" {
			return plan
		}
		newest, err := time.Parse("2006-01-02", previous.Newest)
		if err != nil {
			return plan
		}
		boundary := newest.AddDate(0, 0, -diaryLookbackDays).Format("2006-01-02")
		plan.incremental = true
		plan.stop = func(page letterboxd.ListPage) bool {
			return len(page.Films) > 0 && page.Films[len(page.Films)-1].WatchedOn < boundary
		}
	}
	return plan
}

// nextCursor is the cursor a finished ratings or diary read hands back.
// films is the read in the site's order, before diary entries are combined.
func (p *Provider) nextCursor(snap *snapshot, films []letterboxd.Poster) string {
	next := snap.plan.previous
	if !snap.plan.incremental {
		next.Full = p.now().Unix()
	}
	switch snap.kind {
	case letterboxd.ListRatings:
		next.Head = nil
		for i := 0; i < len(films) && i < ratingsHeadSize; i++ {
			next.Head = append(next.Head, headKey(films[i]))
		}
	case letterboxd.ListDiary:
		if len(films) > 0 && films[0].WatchedOn > next.Newest {
			next.Newest = films[0].WatchedOn
		}
	}
	return next.encode()
}
