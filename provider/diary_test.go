package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

const (
	kindWatched   = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED
	opMarkWatched = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED
)

// stockholm is a fixed UTC+1 zone, so tests do not depend on the machine's.
var stockholm = time.FixedZone("UTC+1", 60*60)

// diaryNow is when the tests' diary export starts.
var diaryNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// newDiaryHarness is a harness whose connection has diary logging on.
func newDiaryHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	h := newHarness(t, append([]func(*Options){func(o *Options) {
		o.Location = stockholm
		o.Now = func() time.Time { return diaryNow }
	}}, opts...)...)
	h.settings = map[string]string{SettingLogPlays: "true"}
	return h
}

func playEvent(id string, ids map[string]string, at time.Time) *pluginv1.WatchSyncEvent {
	event := movieEvent(id, opMarkWatched, "", ids)
	event.WatchHistoryId = id
	event.Completed = true
	event.OccurredAt = timestamppb.New(at)
	return event
}

func statuses(resp *pluginv1.WatchSyncApplyEventsResponse) map[string]pluginv1.WatchSyncApplyStatus {
	out := map[string]pluginv1.WatchSyncApplyStatus{}
	for _, r := range resp.GetResults() {
		out[r.GetEventId()] = r.GetStatus()
	}
	return out
}

// A diary read gives one row per film with its entry count and newest date,
// at the start of that day in the plugin's time zone. It is history, not a
// snapshot, and leaves out TV titles and unlinked films with warnings.
func TestListDiary(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetPageSize(2)
	h.site.SetDiary(
		fakesite.DiaryEntry{LID: "aA1", Date: "2026-11-15"},
		fakesite.DiaryEntry{LID: "bB2", Date: "2026-11-10"},
		fakesite.DiaryEntry{LID: "eE5", Date: "2026-11-09"},
		fakesite.DiaryEntry{LID: "aA1", Date: "2025-02-01"},
		fakesite.DiaryEntry{LID: "fF6", Date: "2024-06-30"},
	)
	h.connectCookie()

	items, _, warnings, fault := h.listKind(kindWatched)
	if fault != nil {
		t.Fatal(fault)
	}
	type row struct {
		plays int32
		last  time.Time
	}
	got := map[string]row{}
	for _, item := range items {
		if item.GetWatched() == nil || item.GetRating() != nil || item.GetWatchlist() != nil {
			t.Errorf("%s: want a watched state only, got %v", item.GetProviderItemKey(), item)
		}
		got[item.GetProviderItemKey()] = row{item.GetWatched().GetPlayCount(), item.GetWatched().GetLastWatchedAt().AsTime()}
	}
	want := map[string]row{
		"letterboxd:aA1": {2, time.Date(2026, 11, 15, 0, 0, 0, 0, stockholm)},
		"letterboxd:bB2": {1, time.Date(2026, 11, 10, 0, 0, 0, 0, stockholm)},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for key, w := range want {
		if got[key].plays != w.plays || !got[key].last.Equal(w.last) {
			t.Errorf("%s = %+v, want %+v", key, got[key], w)
		}
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "Skipped the diary entries of 1 Letterboxd film listed as TV") ||
		!strings.Contains(joined, "Skipped 1 film that has no TMDB or IMDb link") {
		t.Errorf("warnings = %q", warnings)
	}
	h.assertNoSignIn()
}

func TestListEmptyDiary(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	items, _, _, fault := h.listKind(kindWatched)
	if fault != nil || len(items) != 0 {
		t.Fatalf("items %v fault %v; want no rows", items, fault)
	}
}

// A diary read is not a snapshot, so a film whose page is gone is skipped
// with a warning instead of failing every later sync.
func TestListDiarySkipsAFilmWhosePageIsGone(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetDiary(
		fakesite.DiaryEntry{LID: "aA1", Date: "2026-11-15"},
		fakesite.DiaryEntry{LID: "cC3", Date: "2026-11-14"},
	)
	h.site.RemoveFilmPage("a-long-winter")
	h.connectCookie()
	items, _, warnings, fault := h.listKind(kindWatched)
	if fault != nil || strings.Join(keys(items), ",") != "letterboxd:aA1" {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "A Long Winter (1998)") {
		t.Errorf("warnings = %q, want the skipped film named", warnings)
	}
}

// Plays from more than a day before the account's diary export started are
// not logged. Later plays are, once, dated in the plugin's time zone. That
// includes the play whose event started the export, which finished before it
// reached the plugin.
func TestLogPlaysOnlyFromTheStart(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	ids := map[string]string{"tmdb": "5550001"}
	late := time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC) // 00:30 on 17 November in UTC+1

	resp := h.apply(
		playEvent("old", ids, time.Date(2025, 3, 1, 20, 0, 0, 0, time.UTC)),
		playEvent("day-before", map[string]string{"tmdb": "5550002"}, diaryNow.Add(-diaryGrace-time.Second)),
		playEvent("triggering", map[string]string{"tmdb": "5550003"}, diaryNow.Add(-3*time.Second)),
		playEvent("new", ids, late),
	)
	if resp.GetFault() != nil {
		t.Fatal(resp.GetFault())
	}
	got := statuses(resp)
	if got["old"] != statusNoChange || got["day-before"] != statusNoChange ||
		got["triggering"] != statusApplied || got["new"] != statusApplied {
		t.Fatalf("statuses = %v", got)
	}
	diary := h.site.Diary()
	if len(diary) != 2 || diary[0].LID != "aA1" || diary[0].Date != "2026-11-17" ||
		diary[1].LID != "cC3" || diary[1].Date != "2026-11-16" {
		t.Fatalf("diary = %+v, want aA1 on 2026-11-17 and cC3 on 2026-11-16", diary)
	}

	// The same play again, or another play of the film that day, adds nothing.
	resp = h.apply(playEvent("new", ids, late), playEvent("same-day", ids, late.Add(time.Hour)))
	for id, status := range statuses(resp) {
		if status != statusNoChange {
			t.Errorf("%s: %v, want NO_CHANGE", id, status)
		}
	}
	if n := h.site.Logs(); n != 2 {
		t.Fatalf("diary entries added = %d, want 2", n)
	}

	// Reading the entry back gives a time no later than the play, so Silo
	// does not import it as a second, newer play.
	items, _, _, fault := h.listKind(kindWatched)
	if fault != nil || len(items) != 2 {
		t.Fatalf("items %v fault %v", items, fault)
	}
	for _, item := range items {
		if item.GetProviderItemKey() != "letterboxd:aA1" {
			continue
		}
		if back := item.GetWatched().GetLastWatchedAt().AsTime(); back.After(late) {
			t.Fatalf("read back %v, later than the play at %v", back, late)
		}
	}
}

// The start is kept per account in the plugin's instance state, so a restart
// or a new cookie does not move it.
func TestDiaryStartSurvivesRestartAndIsPerAccount(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.apply(playEvent("first", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))

	later := diaryNow.Add(48 * time.Hour)
	restarted := New(Options{BaseURL: h.site.URL, Store: h.store, Location: stockholm, Now: func() time.Time { return later }})
	h.p = restarted
	h.connectCookie() // a new cookie for the same account
	resp := h.apply(playEvent("between", map[string]string{"tmdb": "5550002"}, diaryNow.Add(24*time.Hour)))
	if got := statuses(resp)["between"]; got != statusApplied {
		t.Fatalf("a play after the first start = %v, want APPLIED (the start must not move)", got)
	}

	start, err := restarted.diaryStart.since(context.Background(), "someoneelse")
	if err != nil || !start.Equal(later.Truncate(time.Second)) {
		t.Fatalf("another account's start = %v (%v), want its own first use", start, err)
	}
}

// An unreadable start is retried, never replaced, so a store outage cannot
// drop plays.
func TestDiaryStartUnreadableRetries(t *testing.T) {
	h := newDiaryHarness(t, func(o *Options) { o.Store = &failingStore{memStore: newMemStore(), failing: true} })
	h.connectCookie()
	resp := h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	r := resp.GetResults()[0]
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("result = %v, want RETRY", r)
	}
	if n := h.site.Logs(); n != 0 {
		t.Fatalf("logged %d entries without a start", n)
	}
}

func TestLogPlayRejections(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	after := diaryNow.Add(time.Hour)
	series := playEvent("series", map[string]string{"tmdb": "5550005"}, after)
	series.Media.MediaType = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	untimed := playEvent("untimed", map[string]string{"tmdb": "5550001"}, after)
	untimed.OccurredAt = nil
	resp := h.apply(
		playEvent("unknown", map[string]string{"tmdb": "9999999"}, after),
		playEvent("tv", map[string]string{"imdb": "tt5550005"}, after),
		series,
		untimed,
	)
	for id, status := range statuses(resp) {
		if status != statusRejected {
			t.Errorf("%s: %v, want REJECTED", id, status)
		}
	}
	if n := h.site.Logs(); n != 0 {
		t.Fatalf("logged %d entries", n)
	}
}

func TestLogPlayWithDeadCookie(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	h.site.ExpireSessions()
	resp := h.apply(playEvent("play", map[string]string{"tmdb": "5550001"}, diaryNow.Add(time.Hour)))
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("resp = %v, want INVALID_CREDENTIAL", resp)
	}
	if n := h.site.Logs(); n != 0 {
		t.Fatalf("logged %d entries", n)
	}
}

// The start is read from the host store once per process, not once per play.
func TestDiaryStartIsReadOnce(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	ids := map[string]string{"tmdb": "5550001"}
	h.apply(playEvent("first", ids, diaryNow.Add(time.Hour)))
	h.apply(
		playEvent("second", map[string]string{"tmdb": "5550002"}, diaryNow.Add(2*time.Hour)),
		playEvent("third", map[string]string{"tmdb": "5550003"}, diaryNow.Add(3*time.Hour)),
	)
	if n := h.store.keyReads[diaryStartKey]; n != 1 {
		t.Fatalf("the start was read %d times for three plays, want 1", n)
	}
	if _, ok := h.store.values[diaryStartKey]; !ok {
		t.Fatal("the start was not stored")
	}
}

// Diary logging is off unless the connection turns it on, because Silo turns
// watched export on for every connection. Plays while it is off are not
// logged later, and turning it on does not log the history before it.
func TestDiaryLoggingIsOffUnlessTurnedOn(t *testing.T) {
	h := newDiaryHarness(t)
	h.connectCookie()
	ids := map[string]string{"tmdb": "5550001"}
	for name, settings := range map[string]map[string]string{
		"host without settings": nil,
		"default":               {SettingLogPlays: "false"},
	} {
		h.settings = settings
		resp := h.apply(playEvent(name, ids, diaryNow.Add(time.Hour)))
		if got := statuses(resp)[name]; got != statusNoChange {
			t.Errorf("%s: %v, want NO_CHANGE", name, got)
		}
	}
	if n := h.site.Logs(); n != 0 || h.site.CountRequests("GET /samplemember/diary/") != 0 {
		t.Fatalf("logged %d entries or read the diary with logging off", n)
	}
	if _, ok := h.store.values[diaryStartKey]; ok {
		t.Fatal("the start was recorded while logging was off")
	}

	h.settings = map[string]string{SettingLogPlays: "true"}
	resp := h.apply(
		playEvent("old", map[string]string{"tmdb": "5550002"}, diaryNow.Add(-30*24*time.Hour)),
		playEvent("new", map[string]string{"tmdb": "5550003"}, diaryNow.Add(2*time.Hour)),
	)
	if got := statuses(resp); got["old"] != statusNoChange || got["new"] != statusApplied {
		t.Fatalf("statuses after turning logging on = %v", got)
	}
}

func entryOn(diary []fakesite.DiaryEntry, lid, date string) (fakesite.DiaryEntry, bool) {
	for _, e := range diary {
		if e.LID == lid && e.Date == date {
			return e, true
		}
	}
	return fakesite.DiaryEntry{}, false
}

// A play is logged as a rewatch exactly when the diary already has an entry
// for the film on or before its date, as the site's own Log form decides.
func TestLogPlayMarksRewatches(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetDiary(
		fakesite.DiaryEntry{LID: "bB2", Date: "2024-05-01"},
		fakesite.DiaryEntry{LID: "cC3", Date: "2026-12-24"}, // logged ahead, after the play
	)
	h.connectCookie()
	at := diaryNow.Add(time.Hour) // 2026-11-16 in UTC+1
	resp := h.apply(
		playEvent("first", map[string]string{"tmdb": "5550001"}, at),
		playEvent("seen-before", map[string]string{"tmdb": "5550002"}, at),
		playEvent("seen-later", map[string]string{"tmdb": "5550003"}, at),
	)
	for id, status := range statuses(resp) {
		if status != statusApplied {
			t.Fatalf("%s: %v", id, status)
		}
	}
	diary := h.site.Diary()
	for lid, want := range map[string]bool{"aA1": false, "bB2": true, "cC3": false} {
		entry, ok := entryOn(diary, lid, "2026-11-16")
		if !ok || entry.Rewatch != want {
			t.Errorf("%s: entry %+v (found %v), want rewatch %v", lid, entry, ok, want)
		}
	}

	// A second Silo play of a film first logged by the plugin is a rewatch.
	resp = h.apply(playEvent("again", map[string]string{"tmdb": "5550001"}, at.Add(48*time.Hour)))
	if got := statuses(resp)["again"]; got != statusApplied {
		t.Fatalf("again: %v", got)
	}
	if entry, ok := entryOn(h.site.Diary(), "aA1", "2026-11-18"); !ok || !entry.Rewatch {
		t.Fatalf("second play: %+v (found %v), want a rewatch", entry, ok)
	}
}

// An answer the plugin cannot read logs the play without the flag rather
// than holding the diary back; a rate limit retries the play unlogged.
func TestRewatchCheckFailures(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetDiary(fakesite.DiaryEntry{LID: "bB2", Date: "2024-05-01"})
	h.connectCookie()
	at := diaryNow.Add(time.Hour)

	h.site.RateLimit("/s/check-viewingable-relation", true)
	resp := h.apply(playEvent("limited", map[string]string{"tmdb": "5550002"}, at))
	if r := resp.GetResults()[0]; r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY ||
		r.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED {
		t.Fatalf("rate limited check: %v", r)
	}
	if n := h.site.Logs(); n != 0 {
		t.Fatalf("logged %d entries while the check was rate limited", n)
	}

	h.site.RateLimit("/s/check-viewingable-relation", false)
	h.site.BreakRelation(true)
	resp = h.apply(playEvent("broken", map[string]string{"tmdb": "5550002"}, at))
	if got := statuses(resp)["broken"]; got != statusApplied {
		t.Fatalf("broken check: %v, want APPLIED", got)
	}
	if entry, ok := entryOn(h.site.Diary(), "bB2", "2026-11-16"); !ok || entry.Rewatch {
		t.Fatalf("entry %+v (found %v), want logged without the flag", entry, ok)
	}

	// A refused check does not fail the connection: the write decides.
	h.site.BreakRelation(false)
	h.site.RelationStatus(http.StatusForbidden)
	resp = h.apply(playEvent("refused", map[string]string{"tmdb": "5550002"}, at.Add(48*time.Hour)))
	if resp.GetFault() != nil || statuses(resp)["refused"] != statusApplied {
		t.Fatalf("refused check: %v", resp)
	}
	if entry, ok := entryOn(h.site.Diary(), "bB2", "2026-11-18"); !ok || entry.Rewatch {
		t.Fatalf("entry %+v (found %v), want logged without the flag", entry, ok)
	}
}

// A film cached by an older plugin version has no UID; its page is read once
// for it, and the UID is cached.
func TestRewatchCheckFillsAMissingUID(t *testing.T) {
	h := newDiaryHarness(t)
	h.site.SetDiary(fakesite.DiaryEntry{LID: "bB2", Date: "2024-05-01"})
	h.connectCookie()
	h.p.loadCache(context.Background())
	h.p.cache.put(cachedFilm{LID: "bB2", Slug: "northern-lights-2021", Title: "Northern Lights", Year: 2021,
		TMDBID: "5550002", TMDBType: "movie", CheckedAt: diaryNow.Unix()})
	at := diaryNow.Add(time.Hour)
	h.apply(playEvent("old-cache", map[string]string{"tmdb": "5550002"}, at))
	if entry, ok := entryOn(h.site.Diary(), "bB2", "2026-11-16"); !ok || !entry.Rewatch {
		t.Fatalf("entry %+v (found %v), want a rewatch", entry, ok)
	}
	if film, ok := h.p.cache.byLIDFresh("bB2"); !ok || film.UID == "" {
		t.Fatalf("cached film = %+v, want its UID", film)
	}
	h.apply(playEvent("cached", map[string]string{"tmdb": "5550002"}, at.Add(48*time.Hour)))
	if n := h.site.CountRequests("GET /film/northern-lights-2021/"); n != 1 {
		t.Fatalf("film page read %d times, want once", n)
	}
}
