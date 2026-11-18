package provider

import (
	"context"
	"slices"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

// newSolvingHarness is a harness whose provider loads challenged public
// pages through the fake site's FlareSolverr, set the way Silo sends the
// plugin's settings.
func newSolvingHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.configureSolver(h.site.SolverURL())
	return h
}

func (h *harness) configureSolver(url string) {
	h.t.Helper()
	value, err := structpb.NewStruct(map[string]any{SettingFlareSolverrURL: url})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: SettingFlareSolverr, Value: value}}); err != nil {
		h.t.Fatal(err)
	}
}

// An address the schema lets through but the plugin cannot use turns
// FlareSolverr off instead of failing every sync.
func TestUnusableFlareSolverrSettingIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.configureSolver("http://flaresolverr:port")
	h.site.SetWatchlist("aA1")
	h.connectCookie()

	items, _, _, fault := h.listKind(pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST)
	if fault != nil || len(items) != 1 {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
}

// With FlareSolverr set, a ratings read whose pages and film lookups are
// challenged is still one complete snapshot.
func TestListRatingsThroughFlareSolverr(t *testing.T) {
	h := newSolvingHarness(t)
	h.site.SetPageSize(1)
	h.site.SetRatings(
		fakesite.Rating{LID: "aA1", Rating: 7},
		fakesite.Rating{LID: "bB2", Rating: 10},
	)
	h.connectCookie()
	root := "/samplemember/films/rated/.5-5/by/rated-date/"
	h.site.Challenge(root, true)
	h.site.Challenge(root+"page/2/", true)
	h.site.Challenge("/film/northern-lights-2021/", true)

	items, _, _, fault := h.listKind(kindRating)
	if fault != nil {
		t.Fatal(fault)
	}
	got := ratingsOf(items)
	if len(got) != 2 || got["letterboxd:aA1"] != 7 || got["letterboxd:bB2"] != 10 {
		t.Fatalf("ratings = %v", got)
	}
	for _, path := range []string{root, root + "page/2/", "/film/northern-lights-2021/"} {
		if !slices.Contains(h.site.Requests(), "SOLVE GET "+path) {
			t.Errorf("%s not loaded through FlareSolverr: %v", path, h.site.Requests())
		}
	}
}

// A challenged watchlist stays a challenge, which pauses the connection, and
// is never read signed out.
func TestListWatchlistChallengeSkipsFlareSolverr(t *testing.T) {
	h := newSolvingHarness(t)
	h.site.SetWatchlist("aA1")
	h.connectCookie()
	h.site.Challenge("/samplemember/watchlist/", true)

	items, _, _, fault := h.listKind(pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST)
	if fault == nil || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED || len(items) != 0 {
		t.Fatalf("items %v fault %v; want a rate-limit fault", keys(items), fault)
	}
	if n := h.site.CountRequests("SOLVE "); n != 0 {
		t.Errorf("FlareSolverr was asked %d times", n)
	}
}
