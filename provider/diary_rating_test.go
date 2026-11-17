package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

// manualClock is a clock a test moves by hand.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *manualClock) Add(d time.Duration)     { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newManualClock(at time.Time) *manualClock { return &manualClock{t: at} }

// todayEntry is the film's diary entry on the play date the diary tests use.
func todayEntry(t *testing.T, h *harness, lid string) fakesite.DiaryEntry {
	t.Helper()
	entry, ok := entryOn(h.site.Diary(), lid, "2026-11-16")
	if !ok {
		t.Fatalf("no entry for %s on 2026-11-16: %+v", lid, h.site.Diary())
	}
	return entry
}

func rate(t *testing.T, h *harness, id, lid string, rating int32) {
	t.Helper()
	op := opSetRating
	if rating == 0 {
		op = opRemoveRating
	}
	resp := h.apply(ratingEvent(id, op, "letterboxd:"+lid, nil, rating))
	if s := resp.GetResults()[0].GetStatus(); resp.GetFault() != nil || (s != statusApplied && s != statusNoChange) {
		t.Fatalf("%s: %v", id, resp)
	}
}

// A play of a film already rated on Letterboxd is logged with that rating, as
// the site's Log form fills it in.
func TestLoggedEntryTakesTheFilmsRating(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetRatings(fakesite.Rating{LID: "bB2", Rating: 8})
	h.site.SetDiary(fakesite.DiaryEntry{LID: "bB2", Date: "2024-05-01", Rating: 8})
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550002"}, diaryNow.Add(time.Hour)))
	if e := todayEntry(t, h, "bB2"); e.Rating != 8 || !e.Rewatch {
		t.Fatalf("entry = %+v, want a rewatch rated 8", e)
	}
}

// A rating given in Silo after the play reaches the entry the plugin logged,
// so the diary reads "watched on this date, rated this". Changing it and back
// leaves the first value; removing it clears both.
func TestRatingAfterThePlayReachesTheEntry(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	if e := todayEntry(t, h, "aA1"); e.Rating != 0 {
		t.Fatalf("entry = %+v, want no rating yet", e)
	}
	for _, step := range []struct {
		id     string
		rating int32
	}{{"rate-7", 7}, {"rate-9", 9}, {"rate-7-again", 7}} {
		rate(t, h, step.id, "aA1", step.rating)
		if e := todayEntry(t, h, "aA1"); e.Rating != int(step.rating) {
			t.Fatalf("%s: entry = %+v", step.id, e)
		}
		if got := h.site.Ratings()["aA1"]; got != int(step.rating) {
			t.Fatalf("%s: film rating = %d", step.id, got)
		}
	}
	rate(t, h, "remove", "aA1", 0)
	if e := todayEntry(t, h, "aA1"); e.Rating != 0 {
		t.Fatalf("entry after removal = %+v", e)
	}
	if _, ok := h.site.Ratings()["aA1"]; ok {
		t.Fatal("film still rated after removal")
	}
}

// Entries that are the member's own are left alone: one changed on the site
// after the plugin logged it, one the member logged, and every entry once
// diary logging is off. The film's rating still changes.
func TestRatingLeavesTheMembersEntriesAlone(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetDiary(fakesite.DiaryEntry{LID: "cC3", Date: "2026-11-16"})
	h.connectCookie()
	h.apply(
		playEvent("edited", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)),
		playEvent("off-later", map[string]string{"tmdb": "5550002"}, diaryNow.Add(time.Hour)),
	)
	h.site.EditEntry(todayEntry(t, h, "aA1").EntryID)

	rate(t, h, "edited", "aA1", 7)
	rate(t, h, "own", "cC3", 7)
	h.settings = map[string]string{SettingLogPlays: "false"}
	rate(t, h, "off", "bB2", 7)

	for _, lid := range []string{"aA1", "bB2", "cC3"} {
		if e := todayEntry(t, h, lid); e.Rating != 0 {
			t.Errorf("%s: entry = %+v, want it left alone", lid, e)
		}
		if got := h.site.Ratings()[lid]; got != 7 {
			t.Errorf("%s: film rating = %d, want 7", lid, got)
		}
	}
}

// A rating given more than a week after the plugin logged the entry changes
// the film's rating only, as rating a film on the site does.
func TestRatingReachesOnlyRecentEntries(t *testing.T) {
	clock := newManualClock(diaryNow)
	h := newDiaryHarness(t, func(o *Options) { o.Now = clock.Now })
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	clock.Add(diaryEntryWindow + time.Hour)
	rate(t, h, "late", "aA1", 7)
	if e := todayEntry(t, h, "aA1"); e.Rating != 0 {
		t.Fatalf("entry = %+v, want it left alone", e)
	}
	if n := h.site.CountRequests("GET /api/v0/log-entry/"); n != 0 {
		t.Fatalf("read %d entries for a late rating", n)
	}
}

// The plugin's record of its entries is in instance state, so a restart keeps
// it; an entry deleted on the site is forgotten.
func TestLoggedEntriesSurviveRestartAndDeletion(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(
		playEvent("kept", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)),
		playEvent("deleted", map[string]string{"tmdb": "5550002"}, diaryNow.Add(time.Hour)),
	)
	h.p = New(Options{BaseURL: h.site.URL, Store: h.store, Location: stockholm, Now: func() time.Time { return diaryNow }})
	var kept []fakesite.DiaryEntry
	for _, e := range h.site.Diary() {
		if e.LID != "bB2" {
			kept = append(kept, e)
		}
	}
	h.site.SetDiary(kept...)

	rate(t, h, "after-restart", "aA1", 6)
	if e := todayEntry(t, h, "aA1"); e.Rating != 6 {
		t.Fatalf("entry after a restart = %+v, want 6", e)
	}
	rate(t, h, "gone", "bB2", 6)
	if got := h.site.Ratings()["bB2"]; got != 6 {
		t.Fatalf("film rating = %d, want 6", got)
	}
	recent, err := h.p.diaryEntries.recent(t.Context(), h.site.Username(), "bB2")
	if err != nil || len(recent) != 0 {
		t.Fatalf("deleted entry still recorded: %v (%v)", recent, err)
	}
}

// A failed entry read retries the rating event before the film is rated, so
// the entry and the film do not end up apart.
func TestEntryReadFailureRetriesTheRating(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	h.site.RateLimit("/api/v0/log-entry/"+todayEntry(t, h, "aA1").EntryID, true)
	resp := h.apply(ratingEvent("rate", opSetRating, "letterboxd:aA1", nil, 7))
	if r := resp.GetResults()[0]; r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("result = %v, want RETRY", r)
	}
	if _, ok := h.site.Ratings()["aA1"]; ok {
		t.Fatal("the film was rated although its entry was not")
	}
}

// A rewatch's rating goes on the newest entry only; the earlier viewing's
// entry keeps the rating it had then.
func TestRatingReachesOnlyTheNewestEntry(t *testing.T) {
	clock := newManualClock(diaryNow)
	h := newDiaryHarness(t, func(o *Options) { o.Now = clock.Now })
	h.connectCookie()
	h.apply(playEvent("monday", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	rate(t, h, "rate-monday", "aA1", 6)
	clock.Add(72 * time.Hour)
	h.apply(playEvent("thursday", map[string]string{"tmdb": "5550001"}, clock.Now()))
	rate(t, h, "rate-thursday", "aA1", 9)

	monday, _ := entryOn(h.site.Diary(), "aA1", "2026-11-16")
	thursday, _ := entryOn(h.site.Diary(), "aA1", "2026-11-19")
	if monday.Rating != 6 || thursday.Rating != 9 || !thursday.Rewatch {
		t.Fatalf("monday %+v, thursday %+v; want 6 and a rewatch rated 9", monday, thursday)
	}
}

// A rating the site derived is not one the member gave, so a new entry does
// not take it.
func TestLoggedEntrySkipsADerivedRating(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetRatings(fakesite.Rating{LID: "bB2", Rating: 6})
	h.site.DeriveRating("bB2")
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550002"}, diaryNow.Add(time.Hour)))
	if e := todayEntry(t, h, "bB2"); e.Rating != 0 {
		t.Fatalf("entry = %+v, want no rating", e)
	}
}

// When the site takes an entry but its answer cannot be read, the play
// counts as logged, and a later rating changes the film only.
func TestUnreadableLogAnswer(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.UnreadableLogAnswer(true)
	h.connectCookie()
	resp := h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	if got := statuses(resp)["play"]; got != statusApplied {
		t.Fatalf("play: %v, want APPLIED", got)
	}
	rate(t, h, "rate", "aA1", 7)
	if e := todayEntry(t, h, "aA1"); e.Rating != 0 {
		t.Fatalf("entry = %+v, want it untouched", e)
	}
	if got := h.site.Ratings()["aA1"]; got != 7 {
		t.Fatalf("film rating = %d, want 7", got)
	}
}

// An entry with a review is never updated, since the update replaces the
// review, even when its whenUpdated looks unchanged.
func TestRatingLeavesAnEntryWithAReview(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	h.site.AddReviewQuietly(todayEntry(t, h, "aA1").EntryID)
	rate(t, h, "rate", "aA1", 7)
	if e := todayEntry(t, h, "aA1"); e.Rating != 0 || !e.Review {
		t.Fatalf("entry = %+v, want its review kept and no rating", e)
	}
}

// An entry deleted between the read and the update is forgotten; the film
// is still rated.
func TestEntryGoneBeforeTheUpdate(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	h.site.EntryPatchStatus(404)
	rate(t, h, "rate", "aA1", 7)
	if got := h.site.Ratings()["aA1"]; got != 7 {
		t.Fatalf("film rating = %d, want 7", got)
	}
	recent, err := h.p.diaryEntries.recent(t.Context(), h.site.Username(), "aA1")
	if err != nil || len(recent) != 0 {
		t.Fatalf("entry still recorded: %v (%v)", recent, err)
	}
}

// With too little of the host's deadline left to start another change, the
// rest of the batch is retried rather than run past the deadline.
func TestApplyEventsStopsNearTheDeadline(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	ctx, cancel := context.WithTimeout(t.Context(), deadlineHeadroom/2)
	defer cancel()
	resp, err := h.p.ApplyEvents(ctx, &pluginv1.WatchSyncApplyEventsRequest{Context: h.authContext(), Events: []*pluginv1.WatchSyncEvent{
		playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)),
		ratingEvent("rate", opSetRating, "letterboxd:aA1", nil, 7),
	}})
	if err != nil || resp.GetFault() != nil || len(resp.GetResults()) != 2 {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
	for _, r := range resp.GetResults() {
		if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
			t.Errorf("%s: %v, want RETRY", r.GetEventId(), r)
		}
	}
	if n := h.site.CountRequests("POST") + h.site.CountRequests("PATCH"); n != 0 {
		t.Fatalf("%d writes started with the deadline nearly spent", n)
	}
}

// A dead cookie makes the entry read answer 403. That must lead to the
// cookie check, so the connection asks for a new cookie instead of retrying
// the rating forever.
func TestRatingAfterTheCookieDiedAsksForANewCookie(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	h.site.ExpireSessions()
	resp := h.apply(ratingEvent("rate", opSetRating, "letterboxd:aA1", nil, 7))
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("resp = %v, want an invalid-credential fault", resp)
	}
}
