package provider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

// manyFilms registers n linked movies, mLID0 to mLID<n-1>.
func manyFilms(h *harness, n int) []string {
	lids := make([]string, n)
	for i := range n {
		lids[i] = fmt.Sprintf("m%d", i)
		h.site.AddFilms(fakesite.Film{
			LID: lids[i], Slug: fmt.Sprintf("film-number-%d", i), Title: fmt.Sprintf("Film %d", i), Year: 2000 + i%20,
			TMDBID: fmt.Sprintf("77%04d", i), TMDBType: "movie",
		})
	}
	return lids
}

func (h *harness) ratingPagesRead() int {
	return h.site.CountRequests("GET /samplemember/films/rated/")
}

func (h *harness) diaryPagesRead() int {
	return h.site.CountRequests("GET /samplemember/diary/films/") - h.site.CountRequests("GET /samplemember/diary/films/for/")
}

// With a cursor, a ratings read stops at the first page that holds a film it
// saw last time with the same rating, and is not a snapshot. A rating changed
// on Letterboxd moves to the top, so the short read still finds it. A removed
// rating reaches Silo with the next full read.
func TestRatingsCursorReadsOnlyNewPages(t *testing.T) {
	clock := newManualClock(time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	h := newHarness(t, func(o *Options) { o.Now = clock.Now })
	lids := manyFilms(h, 30)
	var ratings []fakesite.Rating
	for i, lid := range lids {
		ratings = append(ratings, fakesite.Rating{LID: lid, Rating: 1 + i%10})
	}
	h.site.SetRatings(ratings...)
	h.site.SetPageSize(5)
	h.connectCookie()

	full := h.listCursor(kindRating, "")
	if full.fault != nil || !full.complete || len(full.items) != 30 || full.cursor == "" {
		t.Fatalf("first read: %d items, complete %v, cursor %q, fault %v", len(full.items), full.complete, full.cursor, full.fault)
	}

	before := h.ratingPagesRead()
	quiet := h.listCursor(kindRating, full.cursor)
	if quiet.fault != nil || quiet.complete || len(quiet.items) != 5 {
		t.Fatalf("read with nothing new: %d items, complete %v, fault %v", len(quiet.items), quiet.complete, quiet.fault)
	}
	if n := h.ratingPagesRead() - before; n != 1 {
		t.Fatalf("read %d ratings pages with nothing new, want 1", n)
	}

	// The oldest rating changes on Letterboxd.
	h.apply(ratingEvent("rerate", opSetRating, "letterboxd:m29", nil, 3))
	changed := h.listCursor(kindRating, quiet.cursor)
	if changed.fault != nil || changed.complete || ratingsOf(changed.items)["letterboxd:m29"] != 3 {
		t.Fatalf("changed rating not read: %v, fault %v", ratingsOf(changed.items), changed.fault)
	}

	// A rating removed on Letterboxd is not in a short read, and a short
	// read never reads as a removal; the full read a day later leaves it out
	// of a complete snapshot.
	h.apply(ratingEvent("unrate", opRemoveRating, "letterboxd:m10", nil, 0))
	short := h.listCursor(kindRating, changed.cursor)
	if short.complete {
		t.Fatal("a short read claimed to be a snapshot")
	}
	clock.Add(fullRatingsEvery)
	daily := h.listCursor(kindRating, short.cursor)
	if daily.fault != nil || !daily.complete || len(daily.items) != 29 {
		t.Fatalf("full read: %d items, complete %v, fault %v", len(daily.items), daily.complete, daily.fault)
	}
	if _, ok := ratingsOf(daily.items)["letterboxd:m10"]; ok {
		t.Fatal("the removed rating is still in the full read")
	}
}

// When every film the cursor remembers was re-rated, the read goes to the end
// and is still not a snapshot, since it started as a short read.
func TestRatingsCursorWithNoKnownFilmReadsEverything(t *testing.T) {
	h := newHarness(t)
	lids := manyFilms(h, 8)
	h.site.SetPageSize(3)
	var ratings []fakesite.Rating
	for _, lid := range lids {
		ratings = append(ratings, fakesite.Rating{LID: lid, Rating: 6})
	}
	h.site.SetRatings(ratings...)
	h.connectCookie()
	full := h.listCursor(kindRating, "")
	for i := range ratings {
		ratings[i].Rating = 7
	}
	h.site.SetRatings(ratings...)
	again := h.listCursor(kindRating, full.cursor)
	if again.fault != nil || again.complete || len(again.items) != 8 {
		t.Fatalf("%d items, complete %v, fault %v", len(again.items), again.complete, again.fault)
	}
	for key, rating := range ratingsOf(again.items) {
		if rating != 7 {
			t.Errorf("%s = %d, want 7", key, rating)
		}
	}
}

// A cursor the plugin cannot read, from a newer or older version or another
// kind, means a full read.
func TestUnreadableCursorMeansFullRead(t *testing.T) {
	h := newHarness(t)
	h.site.SetRatings(fakesite.Rating{LID: "aA1", Rating: 7}, fakesite.Rating{LID: "bB2", Rating: 8})
	h.connectCookie()
	for _, cursor := range []string{"not json", `{"v":2,"full":1,"head":["aA1:7"]}`, `{"v":1,"full":0}`, `{"v":1,"full":1999999999,"newest":"2026-11-01"}`} {
		r := h.listCursor(kindRating, cursor)
		if r.fault != nil || !r.complete || len(r.items) != 2 {
			t.Errorf("cursor %q: %d items, complete %v, fault %v", cursor, len(r.items), r.complete, r.fault)
		}
	}
}

// With a cursor, a diary read stops at the first page that reaches two weeks
// before the newest entry it saw last time. An entry logged late for an older
// date comes with the weekly full read.
func TestDiaryCursorReadsOnlyRecentPages(t *testing.T) {
	clock := newManualClock(diaryNow)
	h := newDiaryHarness(t, func(o *Options) { o.Now = clock.Now })
	lids := manyFilms(h, 12)
	var entries []fakesite.DiaryEntry
	day := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for i, lid := range lids {
		entries = append(entries, fakesite.DiaryEntry{LID: lid, Date: day.AddDate(0, 0, -5*i).Format("2006-01-02")})
	}
	h.site.SetDiary(entries...)
	h.site.SetPageSize(2)
	h.connectCookie()

	full := h.listCursor(kindWatched, "")
	if full.fault != nil || len(full.items) != 12 || full.cursor == "" {
		t.Fatalf("first read: %d items, cursor %q, fault %v", len(full.items), full.cursor, full.fault)
	}

	// A new entry, and one logged now for a date two months back.
	h.site.SetDiary(append(entries,
		fakesite.DiaryEntry{LID: "aA1", Date: "2026-11-16"},
		fakesite.DiaryEntry{LID: "bB2", Date: "2026-09-10"},
	)...)
	before := h.diaryPagesRead()
	recent := h.listCursor(kindWatched, full.cursor)
	if recent.fault != nil {
		t.Fatal(recent.fault)
	}
	got := strings.Join(keys(recent.items), ",")
	if !strings.Contains(got, "letterboxd:aA1") || strings.Contains(got, "letterboxd:bB2") {
		t.Fatalf("short read = %s, want the new entry and not the late one", got)
	}
	// Newest first: 11-16, 11-15 | 11-10, 11-05 | 10-31 (older than 11-01)
	if n := h.diaryPagesRead() - before; n != 3 {
		t.Fatalf("read %d diary pages, want 3", n)
	}

	clock.Add(fullDiaryEvery)
	weekly := h.listCursor(kindWatched, recent.cursor)
	if weekly.fault != nil || !strings.Contains(strings.Join(keys(weekly.items), ","), "letterboxd:bB2") {
		t.Fatalf("full read = %v, fault %v; want the late entry", keys(weekly.items), weekly.fault)
	}
}

// A short ratings read skips a film whose page is gone, with a warning, since
// it is not a snapshot; a full read still fails rather than let Silo read the
// film as unrated.
func TestRatingsMissingFilmPageSkippedOnlyInShortReads(t *testing.T) {
	clock := newManualClock(time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	h := newHarness(t, func(o *Options) { o.Now = clock.Now })
	h.site.SetRatings(fakesite.Rating{LID: "aA1", Rating: 7}, fakesite.Rating{LID: "bB2", Rating: 8})
	h.connectCookie()
	full := h.listCursor(kindRating, "")
	if full.fault != nil {
		t.Fatal(full.fault)
	}

	h.site.RemoveFilmPage("a-long-winter")
	h.apply(ratingEvent("rate", opSetRating, "letterboxd:cC3", nil, 9))
	short := h.listCursor(kindRating, full.cursor)
	if short.fault != nil || short.complete {
		t.Fatalf("short read: complete %v, fault %v", short.complete, short.fault)
	}
	if _, ok := ratingsOf(short.items)["letterboxd:cC3"]; ok {
		t.Fatal("a film without a readable page was returned")
	}
	if !strings.Contains(strings.Join(short.warnings, "\n"), "Skipped the rating of 1 film whose Letterboxd page was not found") {
		t.Errorf("warnings = %q", short.warnings)
	}

	clock.Add(fullRatingsEvery)
	if daily := h.listCursor(kindRating, short.cursor); daily.fault == nil || len(daily.items) != 0 {
		t.Fatalf("full read: %d items, fault %v; want a fault", len(daily.items), daily.fault)
	}
}
