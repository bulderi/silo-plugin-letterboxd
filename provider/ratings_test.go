package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

const (
	kindRating     = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING
	opSetRating    = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING
	opRemoveRating = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING

	statusApplied  = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED
	statusNoChange = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE
	statusRejected = pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
)

func ratingEvent(id string, op pluginv1.WatchSyncOperation, providerKey string, ids map[string]string, rating int32) *pluginv1.WatchSyncEvent {
	event := movieEvent(id, op, providerKey, ids)
	event.Rating = rating
	return event
}

func ratingsOf(items []*pluginv1.WatchSyncRemoteState) map[string]int32 {
	out := map[string]int32{}
	for _, item := range items {
		out[item.GetProviderItemKey()] = item.GetRating().GetRating()
	}
	return out
}

// A ratings read is one complete snapshot across pages, each film carrying
// the member's 1-to-10 rating and no watchlist state. TV titles and films
// without links are left out and named in warnings.
func TestListRatingsAcrossPages(t *testing.T) {
	h := newHarness(t)
	h.site.SetPageSize(2)
	h.site.SetRatings(
		fakesite.Rating{LID: "aA1", Rating: 7},
		fakesite.Rating{LID: "eE5", Rating: 4},
		fakesite.Rating{LID: "bB2", Rating: 10},
		fakesite.Rating{LID: "fF6", Rating: 6},
		fakesite.Rating{LID: "cC3", Rating: 1},
	)
	h.site.SetWatchlist("dD4")
	h.connectCookie()

	items, _, warnings, fault := h.listKind(kindRating)
	if fault != nil {
		t.Fatal(fault)
	}
	got := ratingsOf(items)
	want := map[string]int32{"letterboxd:aA1": 7, "letterboxd:bB2": 10, "letterboxd:cC3": 1}
	if len(got) != len(want) {
		t.Fatalf("ratings = %v, want %v", got, want)
	}
	for key, rating := range want {
		if got[key] != rating {
			t.Errorf("%s = %d, want %d", key, got[key], rating)
		}
	}
	for _, item := range items {
		if item.GetWatchlist() != nil || item.GetRating() == nil {
			t.Errorf("%s: want a rating state only, got %v", item.GetProviderItemKey(), item)
		}
		if item.GetMedia().GetExternalIds()["tmdb"] == "" {
			t.Errorf("%s: no TMDB id", item.GetProviderItemKey())
		}
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "Skipped the rating of 1 Letterboxd film listed as TV") ||
		!strings.Contains(joined, "Skipped 1 film that has no TMDB or IMDb link") {
		t.Errorf("warnings = %q", warnings)
	}
	if n := h.site.CountRequests("GET /samplemember/watchlist/"); n != 0 {
		t.Errorf("a ratings read loaded the watchlist %d times", n)
	}
	h.assertNoSignIn()
}

func TestListEmptyRatings(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	items, _, _, fault := h.listKind(kindRating)
	if fault != nil || len(items) != 0 {
		t.Fatalf("items %v fault %v; want an empty complete snapshot", items, fault)
	}
}

// A ratings read that does not add up never returns a partial list, because
// Silo reads a rating missing from a complete snapshot as removed.
func TestListRatingsFailuresNeverReturnPartialLists(t *testing.T) {
	cases := map[string]func(h *harness){
		"count too high": func(h *harness) { h.site.ReportRatedCount(4) },
		"count too low":  func(h *harness) { h.site.ReportRatedCount(2) },
		"second page challenged": func(h *harness) {
			h.site.Challenge("/samplemember/films/rated/.5-5/by/rated-date/page/2/", true)
		},
		"film page gone": func(h *harness) { h.site.RemoveFilmPage("a-long-winter") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.site.SetPageSize(2)
			h.site.SetRatings(
				fakesite.Rating{LID: "aA1", Rating: 7},
				fakesite.Rating{LID: "bB2", Rating: 8},
				fakesite.Rating{LID: "cC3", Rating: 9},
			)
			h.connectCookie()
			setup(h)
			items, _, _, fault := h.listKind(kindRating)
			if fault == nil || len(items) != 0 {
				t.Fatalf("items %v fault %v; want a fault and no items", keys(items), fault)
			}
		})
	}
}

// Every Silo rating from 1 to 10 reaches Letterboxd as the same half-star
// rating and reads back unchanged.
func TestSetRatingRoundTripsEveryValue(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	for rating := int32(1); rating <= 10; rating++ {
		resp := h.apply(ratingEvent("set", opSetRating, "letterboxd:aA1", nil, rating))
		if resp.GetFault() != nil || resp.GetResults()[0].GetStatus() != statusApplied {
			t.Fatalf("rating %d: %v", rating, resp)
		}
		if got := h.site.Ratings()["aA1"]; got != int(rating) {
			t.Fatalf("Letterboxd holds %d after Silo set %d", got, rating)
		}
		items, _, _, fault := h.listKind(kindRating)
		if fault != nil || ratingsOf(items)["letterboxd:aA1"] != rating {
			t.Fatalf("read back %v (fault %v) after setting %d", ratingsOf(items), fault, rating)
		}
	}
	if !h.site.Watched("aA1") {
		t.Error("rating did not mark the film watched, which rating_export_requires_watched assumes")
	}
}

// Changing a rating and changing it back leaves the first value, and setting
// or removing twice is harmless: the writes are desired state.
func TestRatingSequences(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	ids := map[string]string{"tmdb": "5550004"}
	resp := h.apply(
		ratingEvent("1", opSetRating, "", ids, 6),
		ratingEvent("2", opSetRating, "", ids, 9),
		ratingEvent("3", opSetRating, "", ids, 6),
		ratingEvent("4", opSetRating, "", ids, 6),
	)
	for _, r := range resp.GetResults() {
		if r.GetStatus() != statusApplied {
			t.Errorf("%s: %v", r.GetEventId(), r)
		}
	}
	if got := h.site.Ratings()["dD4"]; got != 6 {
		t.Fatalf("rating after 6 -> 9 -> 6 = %d, want 6", got)
	}
	resp = h.apply(
		ratingEvent("5", opRemoveRating, "", ids, 0),
		ratingEvent("6", opRemoveRating, "", ids, 0),
	)
	for _, r := range resp.GetResults() {
		if s := r.GetStatus(); s != statusApplied && s != statusNoChange {
			t.Errorf("%s: %v, want APPLIED or NO_CHANGE", r.GetEventId(), r)
		}
	}
	if _, ok := h.site.Ratings()["dD4"]; ok {
		t.Fatal("rating still set after removal")
	}
	if lookups := h.site.CountRequests("GET /tmdb/5550004/"); lookups != 1 {
		t.Fatalf("TMDB lookups = %d, want 1 (cached after the first)", lookups)
	}
}

// A removal is never rejected, because Silo retries a rejected removal on
// every run: a film Letterboxd does not have, or lists as TV, has no rating.
// A rating Letterboxd cannot take is rejected without a write.
func TestRatingRejectionsAndHarmlessRemovals(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	resp := h.apply(
		ratingEvent("remove-unknown", opRemoveRating, "", map[string]string{"tmdb": "9999999"}, 0),
		ratingEvent("remove-tv", opRemoveRating, "", map[string]string{"imdb": "tt5550005"}, 0),
		ratingEvent("remove-no-ids", opRemoveRating, "", nil, 0),
		ratingEvent("set-unknown", opSetRating, "", map[string]string{"tmdb": "9999999"}, 7),
		ratingEvent("set-tv", opSetRating, "", map[string]string{"imdb": "tt5550005"}, 7),
		ratingEvent("set-zero", opSetRating, "letterboxd:aA1", nil, 0),
		ratingEvent("set-eleven", opSetRating, "letterboxd:aA1", nil, 11),
	)
	if resp.GetFault() != nil || len(resp.GetResults()) != 7 {
		t.Fatalf("resp = %v", resp)
	}
	want := map[string]pluginv1.WatchSyncApplyStatus{
		"remove-unknown": statusNoChange,
		"remove-tv":      statusNoChange,
		"remove-no-ids":  statusNoChange,
		"set-unknown":    statusRejected,
		"set-tv":         statusRejected,
		"set-zero":       statusRejected,
		"set-eleven":     statusRejected,
	}
	for _, r := range resp.GetResults() {
		if r.GetStatus() != want[r.GetEventId()] {
			t.Errorf("%s: %v, want %v", r.GetEventId(), r, want[r.GetEventId()])
		}
	}
	if n := h.site.CountRequests("PATCH"); n != 0 {
		t.Fatalf("%d writes reached Letterboxd", n)
	}
}

// A cookie Letterboxd no longer accepts fails a rating write the same way it
// fails a watchlist write: Silo asks for a new cookie.
func TestRatingWriteWithDeadCookie(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	h.site.ExpireSessions()
	resp := h.apply(ratingEvent("set", opSetRating, "letterboxd:aA1", nil, 8))
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("resp = %v, want INVALID_CREDENTIAL", resp)
	}
	if _, ok := h.site.Ratings()["aA1"]; ok {
		t.Fatal("a refused write changed the rating")
	}
	h.assertNoSignIn()
}

// A page token from a watchlist traversal cannot continue a ratings read.
func TestRatingsTraversalRefusesWatchlistToken(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PageBudget = 1 })
	h.site.SetWatchlist("aA1", "bB2", "cC3")
	h.connectCookie()
	watchlist := []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST}
	resp, _ := h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: h.authContext(), StateKinds: watchlist})
	if resp.GetNextPageToken() == "" {
		t.Fatal("want more than one page")
	}
	resp, _ = h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: h.authContext(), StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindRating}, PageToken: resp.GetNextPageToken(),
	})
	if resp.GetFault() == nil || len(resp.GetItems()) != 0 {
		t.Fatalf("a ratings read continued a watchlist snapshot: %v", resp)
	}
}

// Every rating carries the time of the read, so a rating that changed on both
// sides since they last agreed, including every differing rating on a first
// sync, goes to Letterboxd rather than being overwritten by Silo's.
func TestRatingsCarryTheReadTime(t *testing.T) {
	readAt := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	h := newHarness(t, func(o *Options) { o.Now = func() time.Time { return readAt } })
	h.site.SetRatings(fakesite.Rating{LID: "aA1", Rating: 7}, fakesite.Rating{LID: "bB2", Rating: 9})
	h.connectCookie()
	items, _, _, fault := h.listKind(kindRating)
	if fault != nil || len(items) != 2 {
		t.Fatalf("items %v fault %v", items, fault)
	}
	for _, item := range items {
		if got := item.GetRating().GetRatedAt(); got == nil || !got.AsTime().Equal(readAt) {
			t.Errorf("%s: rated_at = %v, want the read time %v", item.GetProviderItemKey(), got, readAt)
		}
	}
}

// Cloudflare challenges the front page far more often than film pages, so a
// write for a film whose lookup is cached still gets its CSRF cookie.
func TestSetRatingWithFrontPageChallenged(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	ids := map[string]string{"tmdb": "5550001"}
	if resp := h.apply(ratingEvent("1", opSetRating, "", ids, 6)); resp.GetResults()[0].GetStatus() != statusApplied {
		t.Fatalf("first rating: %v", resp)
	}
	h.site.Challenge("/", true)
	resp := h.apply(ratingEvent("2", opSetRating, "", ids, 9))
	if resp.GetFault() != nil || resp.GetResults()[0].GetStatus() != statusApplied {
		t.Fatalf("rating with the front page challenged: %v", resp)
	}
	if got := h.site.Ratings()["aA1"]; got != 9 {
		t.Fatalf("Letterboxd holds %d, want 9", got)
	}
}
