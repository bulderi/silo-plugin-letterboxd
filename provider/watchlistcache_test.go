package provider

import (
	"strings"
	"testing"
	"time"
)

func (h *harness) watchlistPagesRead() int {
	return h.site.CountRequests("GET /samplemember/watchlist/")
}

// An unchanged watchlist is served from the last full read after one page; a
// film added (first page changes) or removed (count changes) anywhere, or a
// day passing, means every page is read again. The result is always the
// whole list in Letterboxd's order.
func TestUnchangedWatchlistReadsOnePage(t *testing.T) {
	clock := newManualClock(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC))
	h := newHarness(t, func(o *Options) { o.Now = clock.Now })
	h.site.SetPageSize(2)
	h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4", "gG7")
	h.connectCookie()

	read := func(wantPages int, want string) {
		t.Helper()
		before := h.watchlistPagesRead()
		items, _, fault := h.listAll()
		if fault != nil {
			t.Fatal(fault)
		}
		if got := strings.Join(keys(items), ","); got != want {
			t.Fatalf("items = %s, want %s", got, want)
		}
		if n := h.watchlistPagesRead() - before; n != wantPages {
			t.Fatalf("read %d watchlist pages, want %d", n, wantPages)
		}
	}
	all := "letterboxd:aA1,letterboxd:bB2,letterboxd:cC3,letterboxd:dD4,letterboxd:gG7"
	read(3, all)
	read(1, all)

	h.site.SetWatchlist("cC3", "aA1", "bB2", "dD4", "gG7") // re-added: first page changes
	read(3, "letterboxd:cC3,letterboxd:aA1,letterboxd:bB2,letterboxd:dD4,letterboxd:gG7")
	read(1, "letterboxd:cC3,letterboxd:aA1,letterboxd:bB2,letterboxd:dD4,letterboxd:gG7")

	h.site.SetWatchlist("cC3", "aA1", "bB2", "dD4") // removed from the last page: count changes
	read(2, "letterboxd:cC3,letterboxd:aA1,letterboxd:bB2,letterboxd:dD4")

	clock.Add(watchlistFullEvery)
	read(2, "letterboxd:cC3,letterboxd:aA1,letterboxd:bB2,letterboxd:dD4")
}

// The last full read survives a restart through instance state, and a store
// that cannot be read or written only means every read loads every page.
func TestWatchlistSnapshotStore(t *testing.T) {
	h := newHarness(t)
	h.site.SetPageSize(2)
	h.site.SetWatchlist("aA1", "bB2", "cC3")
	h.connectCookie()
	h.listAll()
	h.p = New(Options{BaseURL: h.site.URL, Store: h.store})
	before := h.watchlistPagesRead()
	h.listAll()
	if n := h.watchlistPagesRead() - before; n != 1 {
		t.Fatalf("after a restart read %d pages, want 1", n)
	}

	broken := newHarness(t, func(o *Options) { o.Store = &failingStore{memStore: newMemStore(), failing: true} })
	broken.site.SetPageSize(2)
	broken.site.SetWatchlist("aA1", "bB2", "cC3")
	broken.connectCookie()
	// The first read cannot use the store and loads every page; the read it
	// keeps in memory then serves the next one.
	for _, wantPages := range []int{2, 1} {
		before := broken.watchlistPagesRead()
		if items, _, fault := broken.listAll(); fault != nil || len(items) != 3 {
			t.Fatalf("items %v fault %v", keys(items), fault)
		}
		if n := broken.watchlistPagesRead() - before; n != wantPages {
			t.Fatalf("with an unreadable store read %d pages, want %d", n, wantPages)
		}
	}
}
