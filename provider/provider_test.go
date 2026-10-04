package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

var testFilms = []fakesite.Film{
	{LID: "aA1", Slug: "the-quiet-harbor", Title: "The Quiet Harbor", Year: 2019, TMDBID: "5550001", TMDBType: "movie", IMDbID: "tt5550001"},
	{LID: "bB2", Slug: "northern-lights-2021", Title: "Northern Lights", Year: 2021, TMDBID: "5550002", TMDBType: "movie"},
	{LID: "cC3", Slug: "a-long-winter", Title: "A Long Winter", Year: 1998, TMDBID: "5550003", TMDBType: "movie", IMDbID: "tt5550003"},
	{LID: "dD4", Slug: "paper-moons", Title: "Paper Moons", Year: 2024, TMDBID: "5550004", TMDBType: "movie"},
	{LID: "eE5", Slug: "the-mini-series", Title: "The Mini Series", Year: 2016, TMDBID: "5550005", TMDBType: "tv", IMDbID: "tt5550005"},
	{LID: "fF6", Slug: "unlinked-short", Title: "Unlinked Short", Year: 2023},
	{LID: "gG7", Slug: "glass-orchard", Title: "Glass Orchard", Year: 2012, TMDBID: "5550007", TMDBType: "movie"},
}

// memStore enforces the host's instance-state limits.
type memStore struct {
	mu     sync.Mutex
	values map[string][]byte
	reads  int
}

func newMemStore() *memStore { return &memStore{values: map[string][]byte{}} }

func (m *memStore) Read(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	v, ok := m.values[key]
	return v, ok, nil
}

func (m *memStore) Write(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(key) > 256 || len(value) > 256<<10 {
		return fmt.Errorf("instance state limit exceeded")
	}
	if _, exists := m.values[key]; !exists && len(m.values) >= 256 {
		return fmt.Errorf("instance state key limit exceeded")
	}
	m.values[key] = append([]byte(nil), value...)
	return nil
}

// stepClock advances by step on every read, which makes the page budget
// deterministic: each film lookup costs a fixed number of ticks.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(c.step)
	return now
}

type harness struct {
	t     *testing.T
	site  *fakesite.Site
	store *memStore
	p     *Provider
	creds *pluginv1.WatchSyncCredentials
}

func newHarness(t *testing.T, opts ...func(*Options)) *harness {
	t.Helper()
	site := fakesite.New(t)
	site.AddFilms(testFilms...)
	store := newMemStore()
	o := Options{BaseURL: site.URL, Store: store}
	for _, opt := range opts {
		opt(&o)
	}
	return &harness{t: t, site: site, store: store, p: New(o)}
}

func (h *harness) connect() {
	h.t.Helper()
	resp, err := h.p.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   "letterboxd",
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{ConnectionUsernameKey: h.site.Username()}},
		ApiKey:         h.site.Password(),
	})
	if err != nil || resp.GetFault() != nil {
		h.t.Fatalf("connect: %v %v", err, resp.GetFault())
	}
	h.creds = resp.GetCredentials()
}

func (h *harness) authContext() *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{CapabilityId: "letterboxd", Credentials: h.creds}
}

// persist applies updated credentials the way the host does.
func (h *harness) persist(updated *pluginv1.WatchSyncCredentials) {
	if updated != nil {
		h.creds = updated
	}
}

// listAll runs a full traversal the way the host does and returns the items.
func (h *harness) listAll() ([]*pluginv1.WatchSyncRemoteState, int, *pluginv1.WatchSyncFault) {
	h.t.Helper()
	var items []*pluginv1.WatchSyncRemoteState
	token := ""
	seen := map[string]bool{}
	for pages := 1; ; pages++ {
		resp, err := h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context:    h.authContext(),
			PageToken:  token,
			PageSize:   10,
			StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
		})
		if err != nil {
			h.t.Fatal(err)
		}
		h.persist(resp.GetUpdatedCredentials())
		if resp.GetFault() != nil {
			return nil, pages, resp.GetFault()
		}
		if !resp.GetCompleteSnapshot() {
			h.t.Fatal("watchlist traversal must be a complete snapshot on every page")
		}
		items = append(items, resp.GetItems()...)
		token = resp.GetNextPageToken()
		if token == "" {
			if resp.GetNextCursor() != "" {
				h.t.Fatal("unexpected cursor")
			}
			return items, pages, nil
		}
		if seen[token] {
			h.t.Fatalf("repeated page token %q", token)
		}
		seen[token] = true
		if pages > 100 {
			h.t.Fatal("traversal did not end")
		}
	}
}

func (h *harness) apply(events ...*pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyEventsResponse {
	h.t.Helper()
	resp, err := h.p.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{Context: h.authContext(), Events: events})
	if err != nil {
		h.t.Fatal(err)
	}
	h.persist(resp.GetUpdatedCredentials())
	return resp
}

func keys(items []*pluginv1.WatchSyncRemoteState) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.GetProviderItemKey())
	}
	return out
}

func movieEvent(id string, op pluginv1.WatchSyncOperation, providerKey string, ids map[string]string) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:         id,
		Operation:       op,
		ProviderItemKey: providerKey,
		Media:           &pluginv1.WatchSyncMedia{MediaType: pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ExternalIds: ids},
	}
}

const (
	opAdd    = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST
	opRemove = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST
)

func TestExchangeAPIKey(t *testing.T) {
	h := newHarness(t)
	resp, err := h.p.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{ConnectionUsernameKey: h.site.Email()}},
		ApiKey:         h.site.Password(),
	})
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("%v %v", err, resp.GetFault())
	}
	if got := resp.GetAccount(); got.GetUsername() != h.site.Username() || got.GetExternalSubject() != h.site.Username() {
		t.Fatalf("account = %v, want the canonical username even for an email login", got)
	}
	creds := resp.GetCredentials()
	if creds.GetAccessToken() == "" || creds.GetRefreshToken() == "" {
		t.Fatalf("credentials missing session cookies: %v", creds)
	}
	if attrs := creds.GetSecretAttributes(); attrs[attrUsername] != h.site.Username() || attrs[attrLogin] != h.site.Email() || attrs[attrPassword] != h.site.Password() {
		t.Fatal("credentials must carry username, login and password for re-login")
	}
}

func TestExchangeAPIKeyFaults(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name     string
		username string
		password string
		setup    func()
		want     pluginv1.WatchSyncFaultCode
	}{
		{"missing username", "", "hunter22", nil, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{"missing password", "samplemember", " ", nil, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{"wrong password", "samplemember", "wrong", nil, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL},
		{"challenge", "samplemember", "correct horse", func() { h.site.Challenge("/sign-in/", true) }, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED},
	}
	for _, tc := range cases {
		if tc.setup != nil {
			tc.setup()
		}
		resp, err := h.p.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
			ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{ConnectionUsernameKey: tc.username}},
			ApiKey:         tc.password,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetFault().GetCode() != tc.want || resp.GetCredentials() != nil {
			t.Errorf("%s: fault = %v, want %v and no credentials", tc.name, resp.GetFault(), tc.want)
		}
		if strings.Contains(resp.GetFault().GetSafeMessage(), tc.password) && strings.TrimSpace(tc.password) != "" {
			t.Errorf("%s: fault message leaks the password", tc.name)
		}
	}
}

func TestListWatchlistAcrossPages(t *testing.T) {
	h := newHarness(t)
	h.site.SetPageSize(2)
	h.site.SetWatchlist("gG7", "eE5", "aA1", "fF6", "bB2", "cC3", "dD4")
	h.connect()

	items, _, fault := h.listAll()
	if fault != nil {
		t.Fatal(fault)
	}
	// Newest first; the TV entry and the film without TMDB/IMDb ids are left out.
	want := []string{"letterboxd:gG7", "letterboxd:aA1", "letterboxd:bB2", "letterboxd:cC3", "letterboxd:dD4"}
	if strings.Join(keys(items), ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keys(items), want)
	}
	first := items[1]
	media := first.GetMedia()
	if media.GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE ||
		media.GetTitle() != "The Quiet Harbor" || media.GetYear() != 2019 ||
		media.GetExternalIds()["tmdb"] != "5550001" || media.GetExternalIds()["imdb"] != "tt5550001" ||
		first.GetWatchlist() == nil || first.GetWatchlist().GetRemoved() {
		t.Fatalf("item = %v", first)
	}
	if items[2].GetMedia().GetExternalIds()["imdb"] != "" {
		t.Fatal("a film without an IMDb link must not get one")
	}
}

func TestListWatchlistSplitsSlowLookupsOverPages(t *testing.T) {
	clock := &stepClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), step: time.Second}
	h := newHarness(t, func(o *Options) { o.Now = clock.Now; o.PageBudget = 4 * time.Second })
	h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4", "gG7")
	h.connect()

	items, pages, fault := h.listAll()
	if fault != nil {
		t.Fatal(fault)
	}
	if len(items) != 5 || pages < 2 {
		t.Fatalf("items = %d over %d pages, want 5 over several pages", len(items), pages)
	}
	// With every film cached, the next traversal needs no film pages and one
	// host page.
	before := h.site.CountRequests("GET /film/")
	items, pages, fault = h.listAll()
	if fault != nil || len(items) != 5 || pages != 1 {
		t.Fatalf("cached traversal: %d items over %d pages, fault %v", len(items), pages, fault)
	}
	if after := h.site.CountRequests("GET /film/"); after != before {
		t.Fatalf("cached traversal fetched %d film pages", after-before)
	}
}

// A film removed on Letterboxd after the pages were read must not shift what
// the host receives: the traversal serves the list as it was read.
func TestListWatchlistIsASnapshot(t *testing.T) {
	clock := &stepClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), step: time.Second}
	h := newHarness(t, func(o *Options) { o.Now = clock.Now; o.PageBudget = 3 * time.Second })
	h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4")
	h.connect()

	req := &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    h.authContext(),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
	}
	resp, _ := h.p.ListRemoteState(context.Background(), req)
	if resp.GetFault() != nil || resp.GetNextPageToken() == "" {
		t.Fatalf("first page: %v", resp)
	}
	got := keys(resp.GetItems())
	h.site.SetWatchlist("bB2", "cC3", "dD4") // aA1 removed mid-traversal
	for token := resp.GetNextPageToken(); token != ""; token = resp.GetNextPageToken() {
		req.PageToken = token
		resp, _ = h.p.ListRemoteState(context.Background(), req)
		if resp.GetFault() != nil {
			t.Fatal(resp.GetFault())
		}
		got = append(got, keys(resp.GetItems())...)
	}
	want := "letterboxd:aA1,letterboxd:bB2,letterboxd:cC3,letterboxd:dD4"
	if strings.Join(got, ",") != want {
		t.Fatalf("keys = %v, want %s", got, want)
	}
}

// A film removed while the pages are still being read shifts the later
// pages, so one film is never seen. The entry count catches it and fails the
// read instead of reporting that film as removed.
func TestListWatchlistChangedWhileReadingPages(t *testing.T) {
	clock := &stepClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), step: time.Second}
	h := newHarness(t, func(o *Options) { o.Now = clock.Now; o.PageBudget = time.Second })
	h.site.SetPageSize(2)
	h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4")
	h.connect()

	req := &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    h.authContext(),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
	}
	resp, _ := h.p.ListRemoteState(context.Background(), req)
	if resp.GetFault() != nil || resp.GetNextPageToken() == "" || len(resp.GetItems()) != 0 {
		t.Fatalf("first call should read one page and hand out nothing yet: %v", resp)
	}
	h.site.SetWatchlist("bB2", "cC3", "dD4") // cC3 moves onto page 1, which was already read
	var fault *pluginv1.WatchSyncFault
	for token := resp.GetNextPageToken(); token != "" && fault == nil; token = resp.GetNextPageToken() {
		req.PageToken = token
		resp, _ = h.p.ListRemoteState(context.Background(), req)
		fault = resp.GetFault()
		if len(resp.GetItems()) != 0 {
			t.Fatalf("items handed out from an inconsistent read: %v", keys(resp.GetItems()))
		}
	}
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Fatalf("fault = %v, want TEMPORARY", fault)
	}
}

// Reading a large watchlist's pages also spreads over host pages, so no
// single call runs into the host's deadline, even with every film cached.
func TestListWatchlistReadsPagesAcrossCalls(t *testing.T) {
	clock := &stepClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), step: time.Second}
	h := newHarness(t, func(o *Options) { o.Now = clock.Now; o.PageBudget = 2 * time.Second })
	h.site.SetPageSize(1)
	h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4", "gG7")
	h.connect()
	if _, _, fault := h.listAll(); fault != nil { // fills the film cache
		t.Fatal(fault)
	}
	before := h.site.CountRequests("GET /samplemember/watchlist/")
	resp, _ := h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    h.authContext(),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
	})
	read := h.site.CountRequests("GET /samplemember/watchlist/") - before
	if resp.GetFault() != nil || resp.GetNextPageToken() == "" || read >= 5 {
		t.Fatalf("first call read %d of 5 pages (fault %v); page reads must stop at the budget", read, resp.GetFault())
	}
	items, _, fault := h.listAll()
	if fault != nil || len(items) != 5 {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
}

func TestListEmptyWatchlist(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist()
	h.connect()
	items, _, fault := h.listAll()
	if fault != nil || len(items) != 0 {
		t.Fatalf("items %v fault %v, want an empty complete snapshot", keys(items), fault)
	}
}

// Any failed read must fail the whole traversal instead of returning a
// shorter list, which the host would read as removals.
func TestListWatchlistFailuresNeverReturnPartialLists(t *testing.T) {
	cases := map[string]struct {
		setup func(site *fakesite.Site)
		want  pluginv1.WatchSyncFaultCode
	}{
		"challenge on page 2": {
			func(site *fakesite.Site) { site.Challenge("/samplemember/watchlist/page/2/", true) },
			pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
		},
		"rate limit on page 2": {
			func(site *fakesite.Site) { site.RateLimit("/samplemember/watchlist/page/2/", true) },
			pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
		},
		"challenge on a film page": {
			func(site *fakesite.Site) { site.Challenge("/film/a-long-winter/", true) },
			pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
		},
		"count does not match the pages": {
			func(site *fakesite.Site) { site.ReportCount(9) },
			pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
		},
		"page without films or a count": {
			func(site *fakesite.Site) { site.SetWatchlist(); site.ReportCount(-1) },
			pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
		},
		"film page gone and never cached": {
			func(site *fakesite.Site) { site.RemoveFilmPage("a-long-winter") },
			pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.site.SetPageSize(2)
			h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4")
			h.connect()
			tc.setup(h.site)
			items, _, fault := h.listAll()
			if fault.GetCode() != tc.want || items != nil {
				t.Fatalf("fault = %v, items = %v; want %v and no items", fault, keys(items), tc.want)
			}
		})
	}
}

func TestListWatchlistCachesLookupsBeforeAFailure(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("aA1", "bB2", "cC3")
	h.connect()
	h.site.Challenge("/film/a-long-winter/", true)
	if _, _, fault := h.listAll(); fault == nil {
		t.Fatal("want a fault")
	}
	h.site.Challenge("/film/a-long-winter/", false)
	before := h.site.CountRequests("GET /film/")
	if _, _, fault := h.listAll(); fault != nil {
		t.Fatal(fault)
	}
	if fetched := h.site.CountRequests("GET /film/") - before; fetched != 1 {
		t.Fatalf("retry fetched %d film pages, want only the one that failed", fetched)
	}
}

func TestFilmCachePersistsAcrossRestarts(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("aA1", "bB2", "eE5")
	h.connect()
	if _, _, fault := h.listAll(); fault != nil {
		t.Fatal(fault)
	}
	restarted := New(Options{BaseURL: h.site.URL, Store: h.store})
	h.p = restarted
	before := h.site.CountRequests("GET /film/")
	items, _, fault := h.listAll()
	if fault != nil || len(items) != 2 {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
	if fetched := h.site.CountRequests("GET /film/") - before; fetched != 0 {
		t.Fatalf("restarted plugin fetched %d film pages, want 0 (TV entries are cached too)", fetched)
	}
}

func TestListWatchlistSignsInAgainWhenSessionExpired(t *testing.T) {
	h := newHarness(t)
	h.site.SetPrivate(true)
	h.site.SetWatchlist("aA1")
	h.connect()
	oldSession := h.creds.GetAccessToken()
	h.site.ExpireSessions()
	items, _, fault := h.listAll()
	if fault != nil || len(items) != 1 {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
	if h.creds.GetAccessToken() == oldSession || h.creds.GetSecretAttributes()[attrPassword] == "" {
		t.Fatal("the new session must be returned as updated credentials, keeping the account")
	}
}

func TestListWatchlistPasswordChanged(t *testing.T) {
	h := newHarness(t)
	h.site.SetPrivate(true)
	h.site.SetWatchlist("aA1")
	h.connect()
	h.site.ExpireSessions()
	h.site.SetPassword("new password")
	_, _, fault := h.listAll()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %v, want INVALID_CREDENTIAL", fault)
	}
}

// A public watchlist reads without a session, so an expired session costs no
// sign-in until the plugin writes.
func TestListPublicWatchlistNeedsNoSession(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("aA1", "bB2")
	h.connect()
	h.site.ExpireSessions()
	before := h.site.CountRequests("POST /user/login.do")
	items, _, fault := h.listAll()
	if fault != nil || len(items) != 2 {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
	if n := h.site.CountRequests("POST /user/login.do") - before; n != 0 {
		t.Fatalf("signed in %d times for a public read", n)
	}
}

func TestListRemoteStateRejectsBadRequests(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("aA1")
	h.connect()
	watchlist := []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST}
	cases := map[string]*pluginv1.WatchSyncListRemoteStateRequest{
		"other kind": {Context: h.authContext(), StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING}},
		"no kind":    {Context: h.authContext()},
		"bad token":  {Context: h.authContext(), StateKinds: watchlist, PageToken: "nope"},
		"old token":  {Context: h.authContext(), StateKinds: watchlist, PageToken: "0123:1"},
		"no creds":   {Context: &pluginv1.WatchSyncAuthenticatedContext{}, StateKinds: watchlist},
	}
	for name, req := range cases {
		resp, err := h.p.ListRemoteState(context.Background(), req)
		if err != nil || resp.GetFault() == nil || len(resp.GetItems()) != 0 {
			t.Errorf("%s: resp = %v, err = %v; want a fault and no items", name, resp, err)
		}
	}
}

func TestPageTokenBelongsToOneAccount(t *testing.T) {
	clock := &stepClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), step: time.Second}
	h := newHarness(t, func(o *Options) { o.Now = clock.Now; o.PageBudget = 2 * time.Second })
	h.site.SetWatchlist("aA1", "bB2", "cC3", "dD4")
	h.connect()
	watchlist := []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST}
	resp, _ := h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: h.authContext(), StateKinds: watchlist})
	if resp.GetNextPageToken() == "" {
		t.Fatal("want more than one page")
	}
	other := &pluginv1.WatchSyncCredentials{
		AccessToken:      "x",
		SecretAttributes: map[string]string{attrUsername: "someoneelse", attrPassword: "p"},
	}
	resp, _ = h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    &pluginv1.WatchSyncAuthenticatedContext{Credentials: other},
		StateKinds: watchlist, PageToken: resp.GetNextPageToken(),
	})
	if resp.GetFault() == nil || len(resp.GetItems()) != 0 {
		t.Fatalf("another account read the snapshot: %v", resp)
	}
}

func TestApplyEventsAddAndRemove(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("bB2")
	h.connect()

	resp := h.apply(
		movieEvent("add-1", opAdd, "tmdb:5550001", map[string]string{"tmdb": "5550001", "imdb": "tt5550001"}),
		movieEvent("add-2", opAdd, "imdb:tt5550003", map[string]string{"imdb": "tt5550003"}),
		movieEvent("rm-1", opRemove, "letterboxd:bB2", map[string]string{"tmdb": "5550002"}),
	)
	if resp.GetFault() != nil {
		t.Fatal(resp.GetFault())
	}
	for _, r := range resp.GetResults() {
		if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Errorf("%s: %v", r.GetEventId(), r)
		}
	}
	if got := strings.Join(h.site.Watchlist(), ","); got != "cC3,aA1" {
		t.Fatalf("watchlist = %s, want cC3,aA1", got)
	}
	// The read after the writes agrees with Silo.
	items, _, fault := h.listAll()
	if fault != nil || strings.Join(keys(items), ",") != "letterboxd:cC3,letterboxd:aA1" {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
}

// Adding, removing and adding again leaves the film on the watchlist, and
// removing a film that is not there is harmless.
func TestApplyEventsSequences(t *testing.T) {
	h := newHarness(t)
	h.connect()
	ids := map[string]string{"tmdb": "5550004"}
	resp := h.apply(
		movieEvent("1", opAdd, "", ids),
		movieEvent("2", opRemove, "", ids),
		movieEvent("3", opAdd, "", ids),
		movieEvent("4", opRemove, "", map[string]string{"tmdb": "5550007"}),
	)
	for _, r := range resp.GetResults() {
		if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Errorf("%s: %v", r.GetEventId(), r)
		}
	}
	if got := strings.Join(h.site.Watchlist(), ","); got != "dD4" {
		t.Fatalf("watchlist = %s, want dD4", got)
	}
	if lookups := h.site.CountRequests("GET /tmdb/5550004/"); lookups != 1 {
		t.Fatalf("TMDB lookups = %d, want 1 (cached after the first)", lookups)
	}
}

func TestApplyEventsRejections(t *testing.T) {
	h := newHarness(t)
	h.connect()
	series := movieEvent("series", opAdd, "", map[string]string{"tmdb": "5550005"})
	series.Media.MediaType = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	resp := h.apply(
		movieEvent("unknown", opAdd, "", map[string]string{"tmdb": "9999999"}),
		movieEvent("no-ids", opAdd, "", nil),
		movieEvent("tv-on-letterboxd", opAdd, "", map[string]string{"imdb": "tt5550005"}),
		series,
		movieEvent("favorite", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_FAVORITE, "", map[string]string{"tmdb": "5550001"}),
	)
	if resp.GetFault() != nil || len(resp.GetResults()) != 5 {
		t.Fatalf("resp = %v", resp)
	}
	for _, r := range resp.GetResults() {
		if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
			t.Errorf("%s: %v, want REJECTED", r.GetEventId(), r)
		}
	}
	if len(h.site.Watchlist()) != 0 || h.site.CountRequests("PATCH") != 0 {
		t.Fatal("a rejected event reached Letterboxd")
	}
}

func TestApplyEventsRateLimitPausesTheRest(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.site.RateLimit("/api/v0/me/watchlist/aA1", true)
	resp := h.apply(
		movieEvent("1", opAdd, "letterboxd:aA1", nil),
		movieEvent("2", opAdd, "letterboxd:bB2", nil),
	)
	for _, r := range resp.GetResults() {
		if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY ||
			r.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED ||
			r.GetFault().GetRetryAfter().AsDuration() != 2*time.Minute {
			t.Errorf("%s: %v", r.GetEventId(), r)
		}
	}
	if n := h.site.CountRequests("PATCH"); n != 1 {
		t.Fatalf("PATCH requests = %d, want 1 (the rest wait for the retry)", n)
	}
}

func TestApplyEventsSessionHandling(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.site.RotateSessionOnWrite(true)
	resp := h.apply(movieEvent("1", opAdd, "letterboxd:aA1", nil))
	if resp.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED || resp.GetUpdatedCredentials() == nil {
		t.Fatalf("rotated session not returned: %v", resp)
	}
	h.site.RotateSessionOnWrite(false)

	h.site.ExpireSessions()
	resp = h.apply(movieEvent("2", opAdd, "letterboxd:bB2", nil))
	if resp.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED || resp.GetUpdatedCredentials() == nil {
		t.Fatalf("expired session not renewed: %v", resp)
	}

	h.site.ExpireSessions()
	h.site.SetPassword("new password")
	resp = h.apply(movieEvent("3", opAdd, "letterboxd:cC3", nil))
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %v, want INVALID_CREDENTIAL", resp.GetFault())
	}
	if got := strings.Join(h.site.Watchlist(), ","); got != "bB2,aA1" {
		t.Fatalf("watchlist = %s", got)
	}
}

func TestRefreshAndGetAccount(t *testing.T) {
	h := newHarness(t)
	h.connect()
	account, err := h.p.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: h.authContext()})
	if err != nil || account.GetAccount().GetUsername() != h.site.Username() {
		t.Fatalf("account = %v, %v", account, err)
	}
	old := h.creds.GetAccessToken()
	refreshed, err := h.p.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: h.authContext()})
	if err != nil || refreshed.GetFault() != nil || refreshed.GetCredentials().GetAccessToken() == old {
		t.Fatalf("refresh = %v, %v", refreshed, err)
	}
	empty := &pluginv1.WatchSyncAuthenticatedContext{}
	if resp, _ := h.p.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: empty}); resp.GetFault() == nil {
		t.Fatal("refresh without stored account must fault")
	}
}

func TestFilmCacheShardsStayWithinHostLimits(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cache := newFilmCache(store, func() time.Time { return now })
	if err := cache.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		cache.put(cachedFilm{
			LID: fmt.Sprintf("L%05d", i), Slug: fmt.Sprintf("film-number-%d", i), Title: strings.Repeat("Title ", 20),
			TMDBID: fmt.Sprint(100000 + i), TMDBType: "movie", CheckedAt: now.Unix() - int64(i),
		})
	}
	if err := cache.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.values) > cacheShards {
		t.Fatalf("%d keys, want at most %d", len(store.values), cacheShards)
	}
	reloaded := newFilmCache(store, func() time.Time { return now })
	if err := reloaded.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if film, ok := reloaded.byExternal("tmdb", "100042"); !ok || film.LID != "L00042" {
		t.Fatalf("reloaded lookup = %+v, %v", film, ok)
	}
}

func TestFilmCacheExpiry(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cache := newFilmCache(nil, func() time.Time { return now })
	cache.put(cachedFilm{LID: "aA1", TMDBID: "1", TMDBType: "movie", CheckedAt: now.Add(-100 * 24 * time.Hour).Unix()})
	cache.put(cachedFilm{LID: "eE5", TMDBID: "5", TMDBType: "tv", CheckedAt: now.Add(-40 * 24 * time.Hour).Unix()})
	if _, ok := cache.byLIDFresh("aA1"); !ok {
		t.Error("a 100-day-old movie entry should still be fresh")
	}
	if _, ok := cache.byLIDFresh("eE5"); ok {
		t.Error("a 40-day-old non-movie entry should be rechecked")
	}
}

// A film Letterboxd refuses even right after a fresh sign-in is that film's
// problem: the rest of the batch goes through and the account stays valid.
func TestApplyEventsRefusedFilmDoesNotFailBatch(t *testing.T) {
	h := newHarness(t)
	h.connect()
	h.site.RefuseWrite("aA1", true)
	resp := h.apply(
		movieEvent("1", opAdd, "letterboxd:aA1", nil),
		movieEvent("2", opAdd, "letterboxd:bB2", nil),
	)
	if resp.GetFault() != nil {
		t.Fatalf("response fault %v; one refused film must not fail the connection", resp.GetFault())
	}
	got := map[string]pluginv1.WatchSyncApplyStatus{}
	for _, r := range resp.GetResults() {
		got[r.GetEventId()] = r.GetStatus()
	}
	if got["1"] != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY ||
		got["2"] != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("statuses = %v", got)
	}
	if n := h.site.CountRequests("POST /user/login.do"); n != 2 {
		t.Fatalf("sign-ins = %d, want 2 (connect, then one retry for the refusal)", n)
	}
	if strings.Join(h.site.Watchlist(), ",") != "bB2" {
		t.Fatalf("watchlist = %v", h.site.Watchlist())
	}
}

// The CSRF cookie is kept with the session, so a write does not first load
// the front page.
func TestApplyEventsReusesStoredCSRF(t *testing.T) {
	h := newHarness(t)
	h.connect()
	if h.creds.GetSecretAttributes()[attrCSRF] == "" {
		t.Fatal("connect did not store the CSRF cookie")
	}
	resp := h.apply(movieEvent("1", opAdd, "letterboxd:aA1", nil))
	if resp.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("resp = %v", resp)
	}
	if n := h.site.CountRequests("GET /"); n != 1 {
		t.Fatalf("GET requests = %d (%v), want only the connect's sign-in page", n, h.site.Requests())
	}
}

func TestListWatchlistUsesStaleCacheWhenFilmPageGone(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, func(o *Options) { o.Now = func() time.Time { return now } })
	h.site.SetWatchlist("aA1", "bB2")
	h.connect()
	if _, _, fault := h.listAll(); fault != nil {
		t.Fatal(fault)
	}
	now = now.Add(200 * 24 * time.Hour) // past the cache lifetime
	h.site.RemoveFilmPage("the-quiet-harbor")
	items, _, fault := h.listAll()
	if fault != nil || strings.Join(keys(items), ",") != "letterboxd:aA1,letterboxd:bB2" {
		t.Fatalf("items %v fault %v; a film whose page is gone must keep its old ids", keys(items), fault)
	}
}

// failingStore fails reads until healed, like a host that has not bound the
// runtime broker yet.
type failingStore struct {
	*memStore
	failing bool
}

func (f *failingStore) Read(ctx context.Context, key string) ([]byte, bool, error) {
	if f.failing {
		return nil, false, fmt.Errorf("runtime host is not bound yet")
	}
	return f.memStore.Read(ctx, key)
}

// Lookups made while the stored cache could not be read must not overwrite
// the stored entries; they are merged once the store answers.
func TestFilmCacheFlushWaitsForLoad(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := &failingStore{memStore: newMemStore()}
	stored := newFilmCache(store, clock)
	if err := stored.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Two films that share a shard: one stored earlier, one looked up later.
	first, second := "aA1", ""
	for i := 0; second == ""; i++ {
		if lid := fmt.Sprintf("L%d", i); shardOf(lid) == shardOf(first) {
			second = lid
		}
	}
	stored.put(cachedFilm{LID: first, TMDBID: "1", TMDBType: "movie", CheckedAt: now.Unix() - 10})
	if err := stored.flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	store.failing = true
	restarted := newFilmCache(store, clock)
	if err := restarted.load(context.Background()); err == nil {
		t.Fatal("load should fail while the store is unreachable")
	}
	restarted.put(cachedFilm{LID: second, TMDBID: "2", TMDBType: "movie", CheckedAt: now.Unix()})
	// A newer lookup of the stored film must win over the stored copy.
	restarted.put(cachedFilm{LID: first, TMDBID: "1-newer", TMDBType: "movie", CheckedAt: now.Unix()})
	if err := restarted.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.failing = false
	if err := restarted.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	check := newFilmCache(store, clock)
	if err := check.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, lid := range []string{first, second} {
		if _, ok := check.byLIDFresh(lid); !ok {
			t.Errorf("%s missing from the stored cache", lid)
		}
	}
	if film, _ := check.byLIDFresh(first); film.TMDBID != "1-newer" {
		t.Errorf("stored copy overwrote the newer lookup: %+v", film)
	}
}

func TestSnapshotLifetimeAndTokens(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	store := newSnapshotStore(30*time.Minute, func() time.Time { return now })
	snap, err := store.start("samplemember")
	if err != nil {
		t.Fatal(err)
	}
	first := store.nextToken(snap)
	if _, ok := store.resume(first, "someoneelse"); ok {
		t.Fatal("another account resumed the snapshot")
	}
	for i := 0; i < 3; i++ { // in use for 60 minutes in total
		now = now.Add(20 * time.Minute)
		if _, ok := store.resume(first, "samplemember"); !ok {
			t.Fatalf("snapshot in use expired after %d minutes", 20*(i+1))
		}
	}
	second := store.nextToken(snap)
	if _, ok := store.resume(first, "samplemember"); ok {
		t.Fatal("a replayed token was accepted")
	}
	now = now.Add(31 * time.Minute)
	if _, ok := store.resume(second, "samplemember"); ok {
		t.Fatal("an unused snapshot outlived its lifetime")
	}
}
