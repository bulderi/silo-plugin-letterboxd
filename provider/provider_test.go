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
	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
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
	mu       sync.Mutex
	values   map[string][]byte
	reads    int
	keyReads map[string]int
}

func newMemStore() *memStore {
	return &memStore{values: map[string][]byte{}, keyReads: map[string]int{}}
}

func (m *memStore) Read(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	m.keyReads[key]++
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
	// settings are the connection settings the host sends.
	settings map[string]string
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

// connect stores a password connection the way versions before 0.4.0 did, so
// the tests that use it prove those stored connections keep working.
func (h *harness) connect() {
	h.t.Helper()
	creds, err := h.p.signIn(context.Background(), h.site.Username(), h.site.Password())
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	h.creds = creds.proto()
}

// connectCookie connects the way a new connection does and returns the cookie.
func (h *harness) connectCookie() string {
	h.t.Helper()
	cookie := h.site.IssueRememberCookie()
	resp := h.exchange(h.site.Username(), cookie)
	if resp.GetFault() != nil {
		h.t.Fatalf("connect: %v", resp.GetFault())
	}
	h.creds = resp.GetCredentials()
	return cookie
}

func (h *harness) exchange(username, apiKey string) *pluginv1.WatchSyncCredentialResponse {
	h.t.Helper()
	resp, err := h.p.ExchangeAPIKey(context.Background(), &pluginv1.WatchSyncExchangeAPIKeyRequest{
		CapabilityId:   "letterboxd",
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{Values: map[string]string{ConnectionUsernameKey: username}},
		ApiKey:         apiKey,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// assertNoSignIn fails if anything touched the Cloudflare-protected sign-in.
func (h *harness) assertNoSignIn() {
	h.t.Helper()
	if n := h.site.CountRequests("GET /sign-in/") + h.site.CountRequests("POST /user/login.do"); n != 0 {
		h.t.Fatalf("%d requests reached the sign-in page: %v", n, h.site.Requests())
	}
}

func (h *harness) authContext() *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{CapabilityId: "letterboxd", Credentials: h.creds, ConnectionSettings: h.settings}
}

// persist applies updated credentials the way the host does.
func (h *harness) persist(updated *pluginv1.WatchSyncCredentials) {
	if updated != nil {
		h.creds = updated
	}
}

// listAll runs a full watchlist traversal the way the host does and returns
// the items.
func (h *harness) listAll() ([]*pluginv1.WatchSyncRemoteState, int, *pluginv1.WatchSyncFault) {
	h.t.Helper()
	items, pages, _, fault := h.listKind(pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST)
	return items, pages, fault
}

// listKind runs a full traversal of one state kind, without a cursor, the way
// the host does and returns the items and every page's warnings.
func (h *harness) listKind(kind pluginv1.WatchSyncRemoteStateKind) ([]*pluginv1.WatchSyncRemoteState, int, []string, *pluginv1.WatchSyncFault) {
	h.t.Helper()
	r := h.listCursor(kind, "")
	if r.fault == nil {
		// The diary is history and never a snapshot; every list read without
		// a cursor is one.
		if want := kind != pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED; r.complete != want {
			h.t.Fatalf("complete_snapshot = %v on a %v read, want %v", r.complete, kind, want)
		}
	}
	return r.items, r.pages, r.warnings, r.fault
}

type traversal struct {
	items    []*pluginv1.WatchSyncRemoteState
	pages    int
	warnings []string
	fault    *pluginv1.WatchSyncFault
	complete bool
	cursor   string
}

// listCursor runs a traversal with the cursor the host saved, the way the
// host does: the snapshot mode stays the same on every page, and the next
// cursor comes only on the last page.
func (h *harness) listCursor(kind pluginv1.WatchSyncRemoteStateKind, cursor string) traversal {
	h.t.Helper()
	var r traversal
	var warnings []string
	var items []*pluginv1.WatchSyncRemoteState
	token := ""
	seen := map[string]bool{}
	for pages := 1; ; pages++ {
		resp, err := h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context:    h.authContext(),
			Cursor:     cursor,
			PageToken:  token,
			PageSize:   10,
			StateKinds: []pluginv1.WatchSyncRemoteStateKind{kind},
		})
		if err != nil {
			h.t.Fatal(err)
		}
		h.persist(resp.GetUpdatedCredentials())
		r.pages = pages
		if resp.GetFault() != nil {
			r.fault = resp.GetFault()
			return r
		}
		if pages > 1 && resp.GetCompleteSnapshot() != r.complete {
			h.t.Fatal("snapshot mode changed during the traversal")
		}
		r.complete = resp.GetCompleteSnapshot()
		items = append(items, resp.GetItems()...)
		warnings = append(warnings, resp.GetWarnings()...)
		token = resp.GetNextPageToken()
		if token == "" {
			isWatchlist := kind == pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST
			if isWatchlist != (resp.GetNextCursor() == "") {
				h.t.Fatalf("next cursor %q on a %v read", resp.GetNextCursor(), kind)
			}
			r.items, r.warnings, r.cursor = items, warnings, resp.GetNextCursor()
			return r
		}
		if resp.GetNextCursor() != "" {
			h.t.Fatal("a cursor before the last page")
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

func TestExchangeAPIKeyStoresTheCookie(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("aA1")
	cookie := h.site.IssueRememberCookie()
	resp := h.exchange(h.site.Username(), "letterboxd.user="+cookie)
	if resp.GetFault() != nil {
		t.Fatal(resp.GetFault())
	}
	h.assertNoSignIn()
	if got := resp.GetAccount(); got.GetUsername() != h.site.Username() || got.GetExternalSubject() != h.site.Username() {
		t.Fatalf("account = %v", got)
	}
	creds := resp.GetCredentials()
	// Silo refuses credentials without an access token.
	if creds.GetAccessToken() != cookie || creds.GetRefreshToken() != cookie {
		t.Fatalf("access = %q, refresh = %q, want the cookie in both", creds.GetAccessToken(), creds.GetRefreshToken())
	}
	attrs := creds.GetSecretAttributes()
	if attrs[attrUsername] != h.site.Username() {
		t.Fatalf("username attribute = %q", attrs[attrUsername])
	}
	if _, ok := attrs[attrPassword]; ok {
		t.Fatal("a cookie connection must store no password")
	}
	if _, ok := attrs[attrLogin]; ok {
		t.Fatal("a cookie connection must store no login")
	}
	stored, ok := credentialsFromContext(&pluginv1.WatchSyncAuthenticatedContext{Credentials: creds})
	if !ok || stored.session.Remember != cookie {
		t.Fatalf("stored = %+v, ok = %v", stored, ok)
	}
	if stored.session.Current != "" {
		t.Fatal("the remember-me cookie must not be sent as the session cookie")
	}
}

func TestExchangeAPIKeyFaults(t *testing.T) {
	const (
		invalidRequest    = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST
		invalidCredential = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL
		rateLimited       = pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED
	)
	cases := []struct {
		name     string
		username string
		apiKey   string
		setup    func(*fakesite.Site)
		want     pluginv1.WatchSyncFaultCode
	}{
		{"missing username", "", "cookie-value", nil, invalidRequest},
		{"missing cookie", "samplemember", " ", nil, invalidRequest},
		{"email instead of username", "samplemember@example.com", "cookie-value", nil, invalidRequest},
		{"dot segment as username", "..", "cookie-value", nil, invalidRequest},
		{"profile address as username", "https://letterboxd.com/samplemember/", "cookie-value", nil, invalidRequest},
		{"whole cookie header", "samplemember", "a=b; letterboxd.user=cookie-value", nil, invalidRequest},
		{"the session cookie", "samplemember", "letterboxd.user.CURRENT=cookie-value", nil, invalidRequest},
		{"dead cookie or a pasted password", "samplemember", "cookie-value", nil, invalidCredential},
		{"challenge", "samplemember", "cookie-value", func(s *fakesite.Site) { s.Challenge("/settings/", true) }, rateLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h.site)
			}
			resp := h.exchange(tc.username, tc.apiKey)
			if resp.GetFault().GetCode() != tc.want || resp.GetCredentials() != nil {
				t.Fatalf("fault = %v, want %v and no credentials", resp.GetFault(), tc.want)
			}
			if strings.Contains(resp.GetFault().GetSafeMessage(), "cookie-value") {
				t.Fatal("fault message leaks the cookie")
			}
			h.assertNoSignIn()
		})
	}
}

// Reads would go to the typed member's list and writes to the cookie owner's.
func TestExchangeAPIKeyRefusesAnotherMembersCookie(t *testing.T) {
	h := newHarness(t)
	resp := h.exchange("someoneelse", h.site.IssueRememberCookie())
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL || resp.GetCredentials() != nil {
		t.Fatalf("resp = %v", resp)
	}
	if !strings.Contains(resp.GetFault().GetSafeMessage(), "not signed in as") {
		t.Fatalf("message = %q", resp.GetFault().GetSafeMessage())
	}
}

func TestExchangeAPIKeyIgnoresUsernameCase(t *testing.T) {
	h := newHarness(t)
	if resp := h.exchange("SampleMember", h.site.IssueRememberCookie()); resp.GetFault() != nil {
		t.Fatal(resp.GetFault())
	}
}

func TestCookieConnectionReadsAndWrites(t *testing.T) {
	h := newHarness(t)
	h.site.SetPageSize(2)
	h.site.SetPrivate(true)
	h.site.SetWatchlist("gG7", "bB2", "cC3")
	h.connectCookie()

	items, _, fault := h.listAll()
	if fault != nil || strings.Join(keys(items), ",") != "letterboxd:gG7,letterboxd:bB2,letterboxd:cC3" {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
	resp := h.apply(
		movieEvent("add", opAdd, "tmdb:5550001", map[string]string{"tmdb": "5550001"}),
		movieEvent("remove", opRemove, "letterboxd:bB2", nil),
	)
	if resp.GetFault() != nil {
		t.Fatal(resp.GetFault())
	}
	for _, r := range resp.GetResults() {
		if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Errorf("%s: %v", r.GetEventId(), r)
		}
	}
	if got := strings.Join(h.site.Watchlist(), ","); got != "aA1,gG7,cC3" {
		t.Fatalf("watchlist = %s", got)
	}
	h.assertNoSignIn()
}

// A cookie connection cannot sign in again to replace a stale CSRF token, so
// each call loads a fresh one, and doing so is no reason to rewrite the
// stored credentials.
func TestCookieConnectionLoadsAFreshCSRFForEachCall(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	if _, ok := h.creds.GetSecretAttributes()[attrCSRF]; ok {
		t.Fatal("a cookie connection must store no CSRF token")
	}
	for i, film := range []string{"aA1", "bB2"} {
		resp := h.apply(movieEvent(film, opAdd, "letterboxd:"+film, nil))
		if resp.GetFault() != nil || resp.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Fatalf("resp = %v", resp)
		}
		if resp.GetUpdatedCredentials() != nil {
			t.Fatalf("call %d rewrote unchanged credentials: %v", i, resp.GetUpdatedCredentials())
		}
		frontPages := 0
		for _, request := range h.site.Requests() {
			if request == "GET /" {
				frontPages++
			}
		}
		if frontPages != i+1 {
			t.Fatalf("after call %d: %d front page loads, want one per call", i, frontPages)
		}
	}
}

func TestCookieExpiredOnPrivateRead(t *testing.T) {
	h := newHarness(t)
	h.site.SetPrivate(true)
	h.site.SetWatchlist("aA1")
	h.connectCookie()
	h.site.ExpireSessions()
	items, _, fault := h.listAll()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL || items != nil {
		t.Fatalf("fault = %v, items = %v", fault, keys(items))
	}
	if !strings.Contains(fault.GetSafeMessage(), "new one") {
		t.Fatalf("message = %q, want it to ask for a new cookie", fault.GetSafeMessage())
	}
	h.assertNoSignIn()
}

func TestCookieExpiredOnPublicReadStillReads(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("aA1")
	h.connectCookie()
	h.site.ExpireSessions()
	if items, _, fault := h.listAll(); fault != nil || len(items) != 1 {
		t.Fatalf("items %v fault %v", keys(items), fault)
	}
}

func TestCookieExpiredOnWrite(t *testing.T) {
	h := newHarness(t)
	h.site.SetWatchlist("cC3")
	h.connectCookie()
	h.site.ExpireSessions()
	resp := h.apply(
		movieEvent("1", opAdd, "letterboxd:aA1", nil),
		movieEvent("2", opAdd, "letterboxd:bB2", nil),
	)
	if resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %v, want INVALID_CREDENTIAL", resp.GetFault())
	}
	if !strings.Contains(resp.GetFault().GetSafeMessage(), "new one") {
		t.Fatalf("message = %q, want it to ask for a new cookie", resp.GetFault().GetSafeMessage())
	}
	if got := strings.Join(h.site.Watchlist(), ","); got != "cC3" {
		t.Fatalf("watchlist = %s", got)
	}
	h.assertNoSignIn()
}

// One film the site refuses says nothing about the cookie when another write
// in the batch went through, in either order.
func TestCookieRefusedFilmDoesNotFailBatch(t *testing.T) {
	for _, order := range [][]string{{"aA1", "bB2"}, {"bB2", "aA1"}} {
		h := newHarness(t)
		h.connectCookie()
		h.site.RefuseWrite("aA1", true)
		resp := h.apply(
			movieEvent(order[0], opAdd, "letterboxd:"+order[0], nil),
			movieEvent(order[1], opAdd, "letterboxd:"+order[1], nil),
		)
		if resp.GetFault() != nil {
			t.Fatalf("order %v: fault %v", order, resp.GetFault())
		}
		got := map[string]pluginv1.WatchSyncApplyStatus{}
		for _, r := range resp.GetResults() {
			got[r.GetEventId()] = r.GetStatus()
		}
		if got["aA1"] != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY ||
			got["bB2"] != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Fatalf("order %v: statuses = %v", order, got)
		}
		h.assertNoSignIn()
	}
}

// A film refused on its own says nothing about a cookie that still works.
func TestCookieLoneRefusedFilmKeepsConnection(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	h.site.RefuseWrite("aA1", true)
	resp := h.apply(movieEvent("1", opAdd, "letterboxd:aA1", nil))
	if resp.GetFault() != nil || resp.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("resp = %v", resp)
	}
	if n := h.site.CountRequests("PATCH /api/v0/me/watchlist/" + letterboxd.ProbeLID); n != 1 {
		t.Fatalf("cookie checks = %d, want 1", n)
	}
	if len(h.site.Watchlist()) != 0 {
		t.Fatalf("the check changed the watchlist: %v", h.site.Watchlist())
	}
}

// A cookie check that cannot finish is no proof the cookie died.
func TestCookieFailedCheckKeepsConnection(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	h.site.ExpireSessions()
	h.site.Challenge("/api/v0/me/watchlist/"+letterboxd.ProbeLID, true)
	resp := h.apply(movieEvent("1", opAdd, "letterboxd:aA1", nil))
	if resp.GetFault() != nil || resp.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatalf("resp = %v", resp)
	}
}

// A rate limit is not evidence that the cookie died.
func TestCookieRefusalThenRateLimitKeepsConnection(t *testing.T) {
	h := newHarness(t)
	h.connectCookie()
	h.site.RefuseWrite("aA1", true)
	h.site.RateLimit("/api/v0/me/watchlist/bB2", true)
	resp := h.apply(
		movieEvent("1", opAdd, "letterboxd:aA1", nil),
		movieEvent("2", opAdd, "letterboxd:bB2", nil),
	)
	if resp.GetFault() != nil || len(resp.GetResults()) != 2 {
		t.Fatalf("resp = %v", resp)
	}
	if n := h.site.CountRequests("PATCH /api/v0/me/watchlist/" + letterboxd.ProbeLID); n != 0 {
		t.Fatalf("cookie checks = %d during a rate limit, want 0", n)
	}
}

func TestCookieRotationIsPersisted(t *testing.T) {
	h := newHarness(t)
	old := h.connectCookie()
	h.site.RotateRememberOnWrite(true)
	resp := h.apply(movieEvent("1", opAdd, "letterboxd:aA1", nil))
	updated := resp.GetUpdatedCredentials()
	if updated.GetRefreshToken() == "" || updated.GetRefreshToken() == old || updated.GetAccessToken() != updated.GetRefreshToken() {
		t.Fatalf("access = %q, refresh = %q, want the new cookie in both", updated.GetAccessToken(), updated.GetRefreshToken())
	}
	h.site.RotateRememberOnWrite(false)
	second := h.apply(movieEvent("2", opAdd, "letterboxd:bB2", nil))
	if second.GetFault() != nil || second.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("write with the rotated cookie: %v", second)
	}
}

// If the site does start setting a session cookie, it takes over the access
// token and the remember-me cookie stays the refresh token.
func TestCookieConnectionKeepsANewSessionCookie(t *testing.T) {
	h := newHarness(t)
	cookie := h.connectCookie()
	h.site.RotateSessionOnWrite(true)
	resp := h.apply(movieEvent("1", opAdd, "letterboxd:aA1", nil))
	updated := resp.GetUpdatedCredentials()
	if updated.GetAccessToken() == "" || updated.GetAccessToken() == cookie || updated.GetRefreshToken() != cookie {
		t.Fatalf("access = %q, refresh = %q", updated.GetAccessToken(), updated.GetRefreshToken())
	}
	if _, ok := updated.GetSecretAttributes()[attrPassword]; ok {
		t.Fatal("updated credentials must not gain a password")
	}
}

func TestRefreshCookieConnectionReturnsWhatItHas(t *testing.T) {
	h := newHarness(t)
	cookie := h.connectCookie()
	before := len(h.site.Requests())
	resp, err := h.p.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: h.authContext()})
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("refresh = %v, %v", resp, err)
	}
	if resp.GetCredentials().GetAccessToken() != cookie || resp.GetCredentials().GetRefreshToken() != cookie {
		t.Fatalf("credentials = %v", resp.GetCredentials())
	}
	if resp.GetAccount().GetUsername() != h.site.Username() {
		t.Fatalf("account = %v", resp.GetAccount())
	}
	if len(h.site.Requests()) != before {
		t.Fatal("refreshing a cookie connection must not contact Letterboxd")
	}
}

// Password connections stored by earlier versions still carry what re-login
// needs, including the canonical username for an email login.
func TestStoredPasswordConnection(t *testing.T) {
	h := newHarness(t)
	creds, err := h.p.signIn(context.Background(), h.site.Email(), h.site.Password())
	if err != nil {
		t.Fatal(err)
	}
	if creds.account.Username != h.site.Username() {
		t.Fatalf("username = %q, want the canonical name for an email login", creds.account.Username)
	}
	proto := creds.proto()
	if proto.GetAccessToken() == "" || proto.GetRefreshToken() == "" {
		t.Fatalf("credentials missing session cookies: %v", proto)
	}
	if attrs := proto.GetSecretAttributes(); attrs[attrUsername] != h.site.Username() || attrs[attrLogin] != h.site.Email() || attrs[attrPassword] != h.site.Password() {
		t.Fatal("credentials must carry username, login and password for re-login")
	}
	stored, ok := credentialsFromContext(&pluginv1.WatchSyncAuthenticatedContext{Credentials: proto})
	if !ok || stored.session.Current != creds.session.Current || stored.session.Remember != creds.session.Remember {
		t.Fatalf("stored = %+v, ok = %v", stored, ok)
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
	// A film added on Letterboxd changes the first page, so the read loads
	// every page again rather than serving the last full read.
	h.site.SetWatchlist("gG7", "aA1", "bB2", "cC3", "dD4")
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
		"other kind": {Context: h.authContext(), StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS}},
		"two kinds": {Context: h.authContext(), StateKinds: []pluginv1.WatchSyncRemoteStateKind{
			pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST,
			pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING,
		}},
		"no kind":   {Context: h.authContext()},
		"bad token": {Context: h.authContext(), StateKinds: watchlist, PageToken: "nope"},
		"old token": {Context: h.authContext(), StateKinds: watchlist, PageToken: "0123:1"},
		"no creds":  {Context: &pluginv1.WatchSyncAuthenticatedContext{}, StateKinds: watchlist},
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
	snap, err := store.start("samplemember", letterboxd.ListWatchlist)
	if err != nil {
		t.Fatal(err)
	}
	first := store.nextToken(snap)
	if _, ok := store.resume(first, "someoneelse", letterboxd.ListWatchlist); ok {
		t.Fatal("another account resumed the snapshot")
	}
	if _, ok := store.resume(first, "samplemember", letterboxd.ListRatings); ok {
		t.Fatal("a ratings traversal resumed a watchlist snapshot")
	}
	for i := 0; i < 3; i++ { // in use for 60 minutes in total
		now = now.Add(20 * time.Minute)
		if _, ok := store.resume(first, "samplemember", letterboxd.ListWatchlist); !ok {
			t.Fatalf("snapshot in use expired after %d minutes", 20*(i+1))
		}
	}
	second := store.nextToken(snap)
	if _, ok := store.resume(first, "samplemember", letterboxd.ListWatchlist); ok {
		t.Fatal("a replayed token was accepted")
	}
	now = now.Add(31 * time.Minute)
	if _, ok := store.resume(second, "samplemember", letterboxd.ListWatchlist); ok {
		t.Fatal("an unused snapshot outlived its lifetime")
	}
}

// Entries left out of the snapshot, and films served from older data, are
// named in page warnings that Silo shows with the sync run.
func TestListWatchlistReportsSkippedEntries(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, func(o *Options) { o.Now = func() time.Time { return now } })
	h.site.SetWatchlist("aA1", "eE5", "fF6")
	h.connect()
	if _, _, fault := h.listAll(); fault != nil {
		t.Fatal(fault)
	}
	now = now.Add(200 * 24 * time.Hour)
	h.site.RemoveFilmPage("the-quiet-harbor")
	resp, err := h.p.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    h.authContext(),
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST},
	})
	if err != nil || resp.GetFault() != nil || resp.GetNextPageToken() != "" {
		t.Fatalf("resp = %v, %v", resp, err)
	}
	got := strings.Join(resp.GetWarnings(), "\n")
	for _, want := range []string{
		"Skipped 1 Letterboxd watchlist entry listed as TV, not a movie: The Mini Series (2016)",
		"Skipped 1 film that has no TMDB or IMDb link on Letterboxd: Unlinked Short (2023)",
		"Used saved IDs for 1 film whose Letterboxd page was not found: The Quiet Harbor (2019)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings missing %q:\n%s", want, got)
		}
	}
	if keys := keys(resp.GetItems()); strings.Join(keys, ",") != "letterboxd:aA1" {
		t.Fatalf("items = %v", keys)
	}
}

func TestSummarizeNamesStaysUnderTheHostLimit(t *testing.T) {
	var names []string
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("A Fairly Long Film Title Number %d (2001)", i))
	}
	got := summarizeNames("Skipped 40 films that have no TMDB or IMDb link on Letterboxd", names)
	if len(got) > maxWarningBytes || !strings.Contains(got, "more") {
		t.Fatalf("%d bytes: %s", len(got), got)
	}
	long := []string{strings.Repeat("x", 400)}
	if got := summarizeNames("Skipped 1 film", long); len(got) > maxWarningBytes || !strings.Contains(got, "1 not listed") {
		t.Fatalf("oversized name: %q", got)
	}
	if got := summarizeNames("Skipped 2", []string{"A (2001)", "B (2002)"}); got != "Skipped 2: A (2001); B (2002)" {
		t.Fatalf("got %q", got)
	}
}
