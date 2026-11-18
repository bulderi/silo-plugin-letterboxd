// Package fakesite is an in-memory stand-in for letterboxd.com used by tests.
// It serves the same markup the real site does for the pages the plugin
// reads, with invented films and members.
package fakesite

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

//go:embed testdata/*.html
var fixtures embed.FS

// Fixture returns one of the static HTML fixtures.
func Fixture(t testing.TB, name string) []byte {
	t.Helper()
	body, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

const (
	csrfCookie     = "com.xk72.webparts.csrf"
	csrfValue      = "csrf-token-1"
	sessionCookie  = "letterboxd.user.CURRENT"
	rememberCookie = "letterboxd.user"
	// UserAgent is the User-Agent every request must carry.
	UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// Film is a film the site knows about.
type Film struct {
	LID      string
	UID      string
	Slug     string
	Title    string
	Year     int
	TMDBID   string
	TMDBType string // "movie", "tv", or "" for no TMDB link
	IMDbID   string
}

// Site is the fake. Its setters are safe to call between requests.
type Site struct {
	t   testing.TB
	srv *httptest.Server

	// URL is the base URL to point a client at.
	URL string

	mu             sync.Mutex
	username       string
	films          map[string]Film // by slug
	watchlist      []string        // LIDs, newest first
	ratings        map[string]int  // by LID, 1 to 10
	ratingOrder    []string        // LIDs in the ratings page's order
	ratedReported  *int
	watched        map[string]bool // LIDs the member has marked watched
	diary          []DiaryEntry    // newest date first
	viewingN       int
	logs           int // diary entries added through the API
	relationBroken bool
	relationStatus int
	derived        map[string]bool // LIDs whose rating the site derived
	unreadableLog  bool
	entryPatch     int // status every entry update answers, 0 for normal
	pageSize       int
	private        bool
	sessions       map[string]bool
	sessionN       int
	rotate         bool
	remembered     map[string]bool
	rememberN      int
	rotateRemember bool
	challenge      map[string]bool
	rateLimit      map[string]bool
	solver         *httptest.Server
	solverError    string
	refused        map[string]bool
	missing        map[string]bool
	reported       *int
	requests       []string
}

// New starts a fake site for member "samplemember".
func New(t testing.TB) *Site {
	t.Helper()
	s := &Site{
		t:          t,
		username:   "samplemember",
		films:      map[string]Film{},
		pageSize:   28,
		sessions:   map[string]bool{},
		remembered: map[string]bool{},
		challenge:  map[string]bool{},
		rateLimit:  map[string]bool{},
		refused:    map[string]bool{},
		missing:    map[string]bool{},
		ratings:    map[string]int{},
		watched:    map[string]bool{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL
	return s
}

// Username is the member's canonical username.
func (s *Site) Username() string { return s.username }

// AddFilms registers films.
func (s *Site) AddFilms(films ...Film) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range films {
		if f.UID == "" {
			f.UID = "film:" + f.LID
		}
		s.films[f.Slug] = f
	}
}

// SetWatchlist replaces the watchlist (LIDs, newest first).
func (s *Site) SetWatchlist(lids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchlist = append([]string(nil), lids...)
}

// Watchlist returns the watchlist LIDs, newest first.
func (s *Site) Watchlist() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.watchlist...)
}

// Rating is one rated film for SetRatings.
type Rating struct {
	LID    string
	Rating int // 1 to 10
}

// SetRatings replaces the member's ratings, most recently rated first, which
// is the order the ratings pages list them in.
func (s *Site) SetRatings(ratings ...Rating) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ratings = map[string]int{}
	s.ratingOrder = nil
	for _, r := range ratings {
		s.ratings[r.LID] = r.Rating
		s.ratingOrder = append(s.ratingOrder, r.LID)
	}
}

// Ratings returns the member's ratings by LID.
func (s *Site) Ratings() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.ratings))
	for lid, r := range s.ratings {
		out[lid] = r
	}
	return out
}

// Watched reports whether the member has marked a film watched.
func (s *Site) Watched(lid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watched[lid]
}

// DiaryEntry is one diary entry.
type DiaryEntry struct {
	ViewingID string
	// EntryID is the entry's LID, which the API's entry calls take.
	EntryID string
	LID     string
	Date    string // YYYY-MM-DD
	Rewatch bool
	Rating  int // 1 to 10, 0 for none
	Like    bool
	Review  bool
	// Updated counts edits, as whenUpdated changes with each.
	Updated int
}

// SetDiary replaces the member's diary. Entries without a ViewingID get one.
func (s *Site) SetDiary(entries ...DiaryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diary = nil
	for _, e := range entries {
		if e.ViewingID == "" || e.EntryID == "" {
			s.viewingN++
			e.ViewingID = strconv.Itoa(1000 + s.viewingN)
			e.EntryID = "e" + strconv.Itoa(s.viewingN)
		}
		s.diary = append(s.diary, e)
	}
	s.sortDiary()
}

// DeriveRating marks a film's rating as one the site derived, not one the
// member set.
func (s *Site) DeriveRating(lid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.derived == nil {
		s.derived = map[string]bool{}
	}
	s.derived[lid] = true
}

// UnreadableLogAnswer makes a new diary entry's answer a page that is not
// JSON; the entry is still added.
func (s *Site) UnreadableLogAnswer(on bool) { s.mu.Lock(); s.unreadableLog = on; s.mu.Unlock() }

// EntryPatchStatus makes every entry update answer this status, or behave
// normally (0).
func (s *Site) EntryPatchStatus(status int) { s.mu.Lock(); s.entryPatch = status; s.mu.Unlock() }

// AddReviewQuietly gives an entry a review without changing its whenUpdated,
// which the real site does not do; it checks the plugin does not rely on
// whenUpdated alone.
func (s *Site) AddReviewQuietly(entryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.entryIndex(entryID); i >= 0 {
		s.diary[i].Review = true
	}
}

// EditEntry changes an entry the way the member would on the site, by
// liking it, which also changes its whenUpdated.
func (s *Site) EditEntry(entryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.diary {
		if s.diary[i].EntryID == entryID {
			s.diary[i].Like = true
			s.diary[i].Updated++
		}
	}
}

// setFilmRating sets the member's rating of a film, as an entry's rating
// does.
func (s *Site) setFilmRating(lid string, rating int) {
	if rating <= 0 {
		delete(s.ratings, lid)
		s.ratingOrder = without(s.ratingOrder, lid)
		return
	}
	s.ratings[lid] = rating
	s.ratingOrder = append([]string{lid}, without(s.ratingOrder, lid)...)
	s.watched[lid] = true
}

func (s *Site) entryIndex(entryID string) int {
	for i := range s.diary {
		if s.diary[i].EntryID == entryID {
			return i
		}
	}
	return -1
}

// entryJSON is an entry as the API returns it.
func entryJSON(e DiaryEntry) string {
	rating := "null"
	if e.Rating > 0 {
		rating = strconv.FormatFloat(float64(e.Rating)/2, 'f', -1, 64)
	}
	review := ""
	if e.Review {
		review = `"review":{"text":"Still great."},`
	}
	return fmt.Sprintf(`{"id":%q,"production":{"id":%q},"diaryDetails":{"diaryDate":%q,"rewatch":%t},%s"tags2":[],"rating":%s,"like":%t,"whenUpdated":"2026-11-17T00:00:%02dZ","privacyPolicy":"Anyone"}`,
		e.EntryID, e.LID, e.Date, e.Rewatch, review, rating, e.Like, e.Updated)
}

// Diary returns the member's diary, newest date first.
func (s *Site) Diary() []DiaryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DiaryEntry(nil), s.diary...)
}

// BreakRelation makes the rewatch check answer a page that is not JSON.
func (s *Site) BreakRelation(on bool) { s.mu.Lock(); s.relationBroken = on; s.mu.Unlock() }

// RelationStatus makes the rewatch check answer this status, or stop (0).
func (s *Site) RelationStatus(status int) { s.mu.Lock(); s.relationStatus = status; s.mu.Unlock() }

// Logs counts the diary entries added through the site's API.
func (s *Site) Logs() int { s.mu.Lock(); defer s.mu.Unlock(); return s.logs }

func (s *Site) sortDiary() {
	sort.SliceStable(s.diary, func(i, j int) bool { return s.diary[i].Date > s.diary[j].Date })
}

// ReportRatedCount makes ratings pages report n rated films instead of the
// real count; a negative n drops the heading from the page.
func (s *Site) ReportRatedCount(n int) { s.mu.Lock(); s.ratedReported = &n; s.mu.Unlock() }

// SetPageSize sets films per watchlist and ratings page.
func (s *Site) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// SetPrivate makes the watchlist visible only when signed in.
func (s *Site) SetPrivate(private bool) { s.mu.Lock(); s.private = private; s.mu.Unlock() }

// RotateSessionOnWrite makes every watchlist write issue a new session cookie.
func (s *Site) RotateSessionOnWrite(on bool) { s.mu.Lock(); s.rotate = on; s.mu.Unlock() }

// RotateRememberOnWrite makes every watchlist write replace the remember-me
// cookie it was sent.
func (s *Site) RotateRememberOnWrite(on bool) { s.mu.Lock(); s.rotateRemember = on; s.mu.Unlock() }

// ExpireSessions signs every session and remember-me cookie out.
func (s *Site) ExpireSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]bool{}
	s.remembered = map[string]bool{}
}

// IssueRememberCookie returns a remember-me cookie as if the member had signed
// in with a browser. Like the real site, a request carrying only this cookie
// is signed in but gets no session or signed-in-as cookie.
func (s *Site) IssueRememberCookie() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newRemember()
}

// Challenge makes a path answer with a Cloudflare challenge (or stop, on=false).
func (s *Site) Challenge(path string, on bool) { s.mu.Lock(); s.challenge[path] = on; s.mu.Unlock() }

// RateLimit makes a path answer 429 (or stop, on=false).
func (s *Site) RateLimit(path string, on bool) { s.mu.Lock(); s.rateLimit[path] = on; s.mu.Unlock() }

// RefuseWrite makes watchlist writes for a film answer 403 even with a valid
// session (or stop, on=false).
func (s *Site) RefuseWrite(lid string, on bool) { s.mu.Lock(); s.refused[lid] = on; s.mu.Unlock() }

// ReportCount makes watchlist pages report n entries instead of the real
// count; a negative n drops the count from the page.
func (s *Site) ReportCount(n int) { s.mu.Lock(); s.reported = &n; s.mu.Unlock() }

// RemoveFilmPage makes a film's page answer 404 while the film stays on the
// watchlist.
func (s *Site) RemoveFilmPage(slug string) { s.mu.Lock(); s.missing[slug] = true; s.mu.Unlock() }

// Requests returns "METHOD path" for every request so far.
func (s *Site) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// CountRequests counts requests whose "METHOD path" starts with prefix.
func (s *Site) CountRequests(prefix string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (s *Site) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("user-agent") != UserAgent {
		s.t.Errorf("%s %s: unexpected user-agent %q", r.Method, r.URL.Path, r.Header.Get("user-agent"))
	}
	if s.challenge[r.URL.Path] {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(Fixture(s.t, "challenge.html"))
		return
	}
	if s.rateLimit[r.URL.Path] {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	s.route(w, r)
}

// route answers a request that got past the challenge and rate-limit checks.
// The caller holds s.mu.
func (s *Site) route(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrfValue, Path: "/"})
	signedIn := s.signedIn(r)
	path := r.URL.Path
	watchlistRoot := "/" + s.username + "/watchlist/"
	ratingsRoot := "/" + s.username + "/films/rated/.5-5/by/rated-date/"
	diaryRoot := "/" + s.username + "/diary/films/"
	switch {
	case path == "/":
		s.writePage(w, signedIn, "Letterboxd", `<body class="home"></body>`)
	case path == "/settings/":
		s.settingsPage(w, signedIn)
	case path == watchlistRoot || strings.HasPrefix(path, watchlistRoot+"page/"):
		s.watchlistPage(w, path, watchlistRoot, signedIn)
	case path == ratingsRoot || strings.HasPrefix(path, ratingsRoot+"page/"):
		s.ratingsPage(w, path, ratingsRoot, signedIn)
	case strings.HasPrefix(path, diaryRoot+"for/"):
		s.diaryDayPage(w, r, strings.Trim(strings.TrimPrefix(path, diaryRoot+"for/"), "/"), signedIn)
	case path == diaryRoot || strings.HasPrefix(path, diaryRoot+"page/"):
		s.diaryPage(w, path, diaryRoot, signedIn)
	case strings.HasPrefix(path, "/film/"):
		s.filmPage(w, r, strings.Trim(strings.TrimPrefix(path, "/film/"), "/"), signedIn)
	case strings.HasPrefix(path, "/tmdb/") || strings.HasPrefix(path, "/imdb/"):
		s.externalRedirect(w, r, path)
	case strings.HasPrefix(path, "/api/v0/me/watchlist/") && r.Method == http.MethodPatch:
		s.patchWatchlist(w, r, strings.TrimPrefix(path, "/api/v0/me/watchlist/"), signedIn)
	case path == "/s/check-viewingable-relation":
		s.checkRelation(w, r, signedIn)
	case strings.HasPrefix(path, "/api/v0/log-entry/") && r.Method == http.MethodGet:
		s.getLogEntry(w, r, strings.TrimPrefix(path, "/api/v0/log-entry/"), signedIn)
	case strings.HasPrefix(path, "/api/v0/production-log-entry/") && (r.Method == http.MethodPatch || r.Method == http.MethodDelete):
		s.changeLogEntry(w, r, strings.TrimPrefix(path, "/api/v0/production-log-entry/"), signedIn)
	case path == "/api/v0/production-log-entries" && r.Method == http.MethodPost:
		s.postLogEntry(w, r, signedIn)
	case strings.HasPrefix(path, "/api/v0/me/rate/") && r.Method == http.MethodPatch:
		s.patchRating(w, r, strings.TrimPrefix(path, "/api/v0/me/rate/"), signedIn)
	default:
		http.NotFound(w, r)
	}
}

// settingsPage answers 200 to everyone, like the real site, and shows the
// username in a form field only to a signed-in member.
func (s *Site) settingsPage(w http.ResponseWriter, signedIn bool) {
	body := `<body class="settings"></body>`
	if signedIn {
		body = `<body class="settings"><form><input name="username" value="` + s.username + `"></form></body>`
	}
	s.writePage(w, signedIn, "Settings", body)
}

func (s *Site) signedIn(r *http.Request) bool {
	if c, err := r.Cookie(sessionCookie); err == nil && s.sessions[c.Value] {
		return true
	}
	c, err := r.Cookie(rememberCookie)
	return err == nil && s.remembered[c.Value]
}

func (s *Site) newRemember() string {
	s.rememberN++
	value := "remember-" + strconv.Itoa(s.rememberN)
	s.remembered[value] = true
	return value
}

func (s *Site) newSession(w http.ResponseWriter) {
	s.sessionN++
	id := "session-" + strconv.Itoa(s.sessionN)
	s.sessions[id] = true
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/"})
}

func (s *Site) watchlistPage(w http.ResponseWriter, path, root string, signedIn bool) {
	if s.private && !signedIn {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(Fixture(s.t, "forbidden.html"))
		return
	}
	page, start, end, ok := s.pageBounds(path, root, len(s.watchlist))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	count := fmt.Sprintf(` data-num-entries="%d"`, len(s.watchlist))
	if s.reported != nil {
		count = ""
		if *s.reported >= 0 {
			count = fmt.Sprintf(` data-num-entries="%d"`, *s.reported)
		}
	}
	var b strings.Builder
	b.WriteString(`<body class="watchlist"><div class="cols-2 js-watchlist-content"` + count + `><ul class="grid">`)
	for _, lid := range s.watchlist[start:end] {
		f, ok := s.filmByLID(lid)
		if !ok {
			s.t.Errorf("watchlist has unknown LID %q", lid)
			continue
		}
		b.WriteString(`<li class="griditem">` + posterDiv(f) + `</li>`)
	}
	b.WriteString(`</ul></div><div class="pagination">`)
	if end < len(s.watchlist) {
		fmt.Fprintf(&b, `<div class="paginate-nextprev"><a class="next" href="%spage/%d/">Older</a></div>`, root, page+1)
	} else {
		b.WriteString(`<div class="paginate-nextprev paginate-disabled"><span class="next">Older</span></div>`)
	}
	b.WriteString(`</div></body>`)
	s.writePage(w, signedIn, "Watchlist", b.String())
}

// ratingsPage serves /<member>/films/rated/.5-5/by/rated-date/ the way the
// real site does:
// a heading with the number of rated films (or the empty-list message), then
// a grid whose tiles show the member's rating. The heading carries rating
// spans of its own, outside the grid.
func (s *Site) ratingsPage(w http.ResponseWriter, path, root string, signedIn bool) {
	page, start, end, ok := s.pageBounds(path, root, len(s.ratingOrder))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	count := len(s.ratingOrder)
	if s.ratedReported != nil {
		count = *s.ratedReported
	}
	var b strings.Builder
	b.WriteString(`<body class="films-watched">`)
	stars := `<span class="rating -tiny rated-1"> ½ </span> and <span class="rating -tiny rated-10"> ★★★★★ </span>`
	switch {
	case count < 0:
	case count == 0:
		b.WriteString(`<section class="ui-block-header filtered-message body-text -small  empty-text"><p class="ui-block-heading">` +
			`<span class="replace-if-you" data-replacement="You haven’t" data-person="` + s.username + `">Sample hasn’t</span> watched any films rated between ` + stars + `.</p></section>`)
	default:
		films := "films"
		if count == 1 {
			films = "film"
		}
		fmt.Fprintf(&b, `<section class="ui-block-header filtered-message body-text -small "><p class="ui-block-heading">`+
			`<span class="replace-if-you" data-replacement="You’ve" data-person="%s">Sample has</span> watched %s %s rated between %s.</p></section>`,
			s.username, thousands(count), films, stars)
	}
	b.WriteString(`<ul class="grid -p70">`)
	for _, lid := range s.ratingOrder[start:end] {
		f, ok := s.filmByLID(lid)
		if !ok {
			s.t.Errorf("ratings have unknown LID %q", lid)
			continue
		}
		fmt.Fprintf(&b, `<li class="griditem">%s<p class="poster-viewingdata" data-item-uid="%s"> <span class="rating -micro -darker rated-%d">★</span> </p></li>`,
			posterDiv(f), f.UID, s.ratings[lid])
	}
	b.WriteString(`</ul><div class="pagination">`)
	if end < len(s.ratingOrder) {
		fmt.Fprintf(&b, `<div class="paginate-nextprev"><a class="next" href="%spage/%d/">Next</a></div>`, root, page+1)
	}
	b.WriteString(`</div></body>`)
	s.writePage(w, signedIn, "Films", b.String())
}

// diaryPage serves the diary like the real site: one row per entry, newest
// first, each with its date link, film poster, and rating cell.
func (s *Site) diaryPage(w http.ResponseWriter, path, root string, signedIn bool) {
	page, start, end, ok := s.pageBounds(path, root, len(s.diary))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	next := ""
	if end < len(s.diary) {
		next = fmt.Sprintf(`<div class="paginate-nextprev"><a class="next" href="%spage/%d/">Older</a></div>`, root, page+1)
	}
	s.writeDiary(w, signedIn, s.diary[start:end], "any entries for films", next)
}

func (s *Site) diaryDayPage(w http.ResponseWriter, r *http.Request, date string, signedIn bool) {
	day, err := time.Parse("2006/01/02", date)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var entries []DiaryEntry
	for _, e := range s.diary {
		if e.Date == day.Format("2006-01-02") {
			entries = append(entries, e)
		}
	}
	// Like the real site, a day's page links the next day as "next".
	next := fmt.Sprintf(`<div class="paginate-nextprev"><a class="next" href="/%s/diary/for/%s/">Next</a></div>`,
		s.username, day.AddDate(0, 0, 1).Format("2006/01/02"))
	s.writeDiary(w, signedIn, entries, "any entries for films on "+day.Format("2 January 2006"), next)
}

func (s *Site) writeDiary(w http.ResponseWriter, signedIn bool, entries []DiaryEntry, emptyWhat, next string) {
	var b strings.Builder
	b.WriteString(`<body class="diary">`)
	if len(entries) == 0 {
		b.WriteString(`<section class="ui-block-header filtered-message body-text -small  empty-text"><p class="ui-block-heading">Sample hasn’t logged ` + emptyWhat + `.</p></section>`)
	}
	b.WriteString(`<table class="table film-table" id="diary-table"><tbody>`)
	for _, e := range entries {
		f, ok := s.filmByLID(e.LID)
		if !ok {
			s.t.Errorf("diary has unknown LID %q", e.LID)
			continue
		}
		y, m, d := e.Date[0:4], e.Date[5:7], e.Date[8:10]
		fmt.Fprintf(&b, `<tr class="diary-entry-row viewing-poster-container" data-production-uid="%s" data-viewing-id="%s" data-owner="%s" data-object-id="viewing:%s">`+
			`<td class="col-monthdate"><a href="/%s/diary/films/for/%s/%s/">%s</a></td>`+
			`<td class="col-daydate"><a class="daydate" href="/%s/diary/films/for/%s/%s/%s/">%s</a></td>`+
			`<td class="col-production">%s</td><td class="col-rating">%s</td></tr>`,
			f.UID, e.ViewingID, s.username, e.ViewingID, s.username, y, m, m, s.username, y, m, d, d, posterDiv(f), ratingSpan(e.Rating))
	}
	b.WriteString(`</tbody></table><div class="pagination">` + next + `</div></body>`)
	s.writePage(w, signedIn, "Diary", b.String())
}

// checkRelation answers the Log form's question about a film: the member's
// like and rating, and the earliest diary entries, which decide "rewatch".
func (s *Site) checkRelation(w http.ResponseWriter, r *http.Request, signedIn bool) {
	if s.relationStatus != 0 {
		w.WriteHeader(s.relationStatus)
		return
	}
	w.Header().Set("content-type", "application/json;charset=UTF-8")
	if s.relationBroken {
		w.Header().Set("content-type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(w, "<html>maintenance</html>")
		return
	}
	var film Film
	found := false
	for _, f := range s.films {
		if f.UID == r.URL.Query().Get("viewingableUID") {
			film, found = f, true
		}
	}
	if !signedIn || !found {
		_, _ = io.WriteString(w, `{"result":false}`)
		return
	}
	var dates []DiaryEntry
	for _, e := range s.diary {
		if e.LID == film.LID {
			dates = append(dates, e)
		}
	}
	sort.SliceStable(dates, func(i, j int) bool { return dates[i].Date < dates[j].Date })
	earliest, earliestID, second := "null", "null", "null"
	if len(dates) > 0 {
		earliest, earliestID = strconv.Quote(dates[0].Date), strconv.Quote(dates[0].ViewingID)
	}
	if len(dates) > 1 {
		second = strconv.Quote(dates[1].Date)
	}
	rating := "null"
	if r, ok := s.ratings[film.LID]; ok {
		rating = strconv.Itoa(r)
	}
	_, _ = fmt.Fprintf(w, `{"result":true,"liked":false,"rating":%s,"isDerivedRating":%t,"earliestViewingDate":%s,"earliestViewingId":%s,"secondEarliestViewingDate":%s,"isInWatchlist":false}`,
		rating, s.derived[film.LID], earliest, earliestID, second)
}

func ratingSpan(rating int) string {
	if rating <= 0 {
		return ""
	}
	return fmt.Sprintf(`<span class="rating rated-%d"></span>`, rating)
}

func (s *Site) getLogEntry(w http.ResponseWriter, r *http.Request, entryID string, signedIn bool) {
	// Like the real site, a signed-out request is refused before the entry
	// is looked up.
	if !signedIn {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	i := s.entryIndex(entryID)
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", "application/json;charset=UTF-8")
	_, _ = io.WriteString(w, entryJSON(s.diary[i]))
}

// changeLogEntry updates an entry like the site's edit form, which replaces
// the review, tags, rating, and like, or deletes it.
func (s *Site) changeLogEntry(w http.ResponseWriter, r *http.Request, entryID string, signedIn bool) {
	if !signedIn || r.Header.Get("x-csrf-token") != csrfValue {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	i := s.entryIndex(entryID)
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPatch && s.entryPatch != 0 {
		w.WriteHeader(s.entryPatch)
		return
	}
	w.Header().Set("content-type", "application/json;charset=UTF-8")
	if r.Method == http.MethodDelete {
		s.diary = append(s.diary[:i], s.diary[i+1:]...)
		_, _ = io.WriteString(w, `{"relationship":{}}`)
		return
	}
	var req struct {
		DiaryDetails *struct {
			DiaryDate string `json:"diaryDate"`
			Rewatch   bool   `json:"rewatch"`
		} `json:"diaryDetails"`
		Review json.RawMessage `json:"review"`
		Tags   []string        `json:"tags"`
		Rating *float64        `json:"rating"`
		Like   bool            `json:"like"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil || req.DiaryDetails == nil {
		s.t.Errorf("PATCH log entry body = %q", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	e := &s.diary[i]
	e.Date, e.Rewatch, e.Like = req.DiaryDetails.DiaryDate, req.DiaryDetails.Rewatch, req.Like
	e.Review = len(req.Review) > 0 && string(req.Review) != "null"
	e.Rating = 0
	if req.Rating != nil {
		e.Rating = int(*req.Rating * 2)
		s.setFilmRating(e.LID, e.Rating)
	}
	e.Updated++
	s.sortDiary()
	_, _ = io.WriteString(w, `{"logEntry":`+entryJSON(s.diary[s.entryIndex(entryID)])+`}`)
}

// postLogEntry adds a diary entry the way the site's Log form does.
func (s *Site) postLogEntry(w http.ResponseWriter, r *http.Request, signedIn bool) {
	if !signedIn || r.Header.Get("x-csrf-token") != csrfValue {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(Fixture(s.t, "forbidden.html"))
		return
	}
	var req struct {
		ProductionID string `json:"productionId"`
		DiaryDetails *struct {
			DiaryDate string `json:"diaryDate"`
			Rewatch   bool   `json:"rewatch"`
		} `json:"diaryDetails"`
		Tags   []string `json:"tags"`
		Rating *float64 `json:"rating"`
		Like   bool     `json:"like"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil || req.DiaryDetails == nil {
		s.t.Errorf("POST log entry body = %q", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", req.DiaryDetails.DiaryDate); err != nil {
		s.t.Errorf("diaryDate = %q", req.DiaryDetails.DiaryDate)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, ok := s.filmByLID(req.ProductionID); !ok {
		http.NotFound(w, r)
		return
	}
	if s.refused[req.ProductionID] {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.viewingN++
	entry := DiaryEntry{
		ViewingID: strconv.Itoa(1000 + s.viewingN), EntryID: "e" + strconv.Itoa(s.viewingN),
		LID: req.ProductionID, Date: req.DiaryDetails.DiaryDate, Rewatch: req.DiaryDetails.Rewatch,
	}
	if req.Rating != nil {
		entry.Rating = int(*req.Rating * 2)
		s.setFilmRating(entry.LID, entry.Rating)
	}
	s.diary = append(s.diary, entry)
	s.sortDiary()
	s.watched[req.ProductionID] = true
	s.logs++
	if s.unreadableLog {
		w.Header().Set("content-type", "text/html; charset=UTF-8")
		_, _ = io.WriteString(w, "<html>saved</html>")
		return
	}
	w.Header().Set("content-type", "application/json;charset=UTF-8")
	_, _ = io.WriteString(w, `{"logEntry":`+entryJSON(entry)+`,"relationship":{}}`)
}

// pageBounds reads the page number from a list path (root, or root plus
// page/<n>/) and returns the slice of a list of total items it shows; ok is
// false for a page that does not exist.
func (s *Site) pageBounds(path, root string, total int) (page, start, end int, ok bool) {
	page = 1
	if rest := strings.TrimPrefix(path, root+"page/"); rest != path {
		n, err := strconv.Atoi(strings.Trim(rest, "/"))
		if err != nil || n < 1 {
			return 0, 0, 0, false
		}
		page = n
	}
	start = (page - 1) * s.pageSize
	if start > 0 && start >= total {
		return 0, 0, 0, false
	}
	return page, start, min(start+s.pageSize, total), true
}

func thousands(n int) string {
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return b.String()
}

func (s *Site) filmPage(w http.ResponseWriter, r *http.Request, slug string, signedIn bool) {
	f, ok := s.films[slug]
	if !ok || s.missing[slug] {
		http.NotFound(w, r)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<meta property="og:url" content="https://letterboxd.com/film/%s/"><meta property="og:title" content="%s">`,
		f.Slug, html.EscapeString(displayName(f)))
	if f.TMDBType != "" {
		fmt.Fprintf(&b, `<body class="film" data-type="film" data-tmdb-type="%s" data-tmdb-id="%s">`, f.TMDBType, f.TMDBID)
	} else {
		b.WriteString(`<body class="film" data-type="film">`)
	}
	b.WriteString(`<section class="similar"><ul><li class="griditem">` +
		posterDiv(Film{LID: "zZ9", UID: "film:2999", Slug: "some-other-film", Title: "Some Other Film", Year: 2010}) +
		`</li></ul></section>`)
	b.WriteString(`<section class="poster-list">` + posterDiv(f) + `</section>`)
	if f.IMDbID != "" {
		fmt.Fprintf(&b, `<a href="http://www.imdb.com/title/%s/maindetails">IMDb</a>`, f.IMDbID)
	}
	b.WriteString(`</body>`)
	s.writePage(w, signedIn, f.Title, b.String())
}

func (s *Site) externalRedirect(w http.ResponseWriter, r *http.Request, path string) {
	namespace, id, _ := strings.Cut(strings.Trim(path, "/"), "/")
	for _, f := range s.films {
		if (namespace == "tmdb" && f.TMDBID == id && f.TMDBType == "movie") || (namespace == "imdb" && f.IMDbID == id) {
			http.Redirect(w, r, "/film/"+f.Slug+"/", http.StatusFound)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Site) patchWatchlist(w http.ResponseWriter, r *http.Request, lid string, signedIn bool) {
	if !signedIn || r.Header.Get("x-csrf-token") != csrfValue {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(Fixture(s.t, "forbidden.html"))
		return
	}
	if _, ok := s.filmByLID(lid); !ok {
		http.NotFound(w, r)
		return
	}
	if s.refused[lid] {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in bool
	switch string(body) {
	case `{"inWatchlist":true}`:
		in = true
	case `{"inWatchlist":false}`:
	default:
		s.t.Errorf("PATCH body = %q", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kept := s.watchlist[:0:0]
	for _, existing := range s.watchlist {
		if existing != lid {
			kept = append(kept, existing)
		}
	}
	if in {
		kept = append([]string{lid}, kept...)
	}
	s.watchlist = kept
	if s.rotate {
		if c, err := r.Cookie(sessionCookie); err == nil {
			delete(s.sessions, c.Value)
		}
		s.newSession(w)
	}
	if s.rotateRemember {
		if c, err := r.Cookie(rememberCookie); err == nil {
			delete(s.remembered, c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: rememberCookie, Value: s.newRemember(), Path: "/"})
	}
	w.Header().Set("content-type", "application/json;charset=UTF-8")
	_, _ = io.WriteString(w, `{}`)
}

// patchRating handles the site's rating write: {"rating":3.5} sets half
// stars from 0.5 to 5 and marks the film watched, {"rating":null} clears it.
func (s *Site) patchRating(w http.ResponseWriter, r *http.Request, lid string, signedIn bool) {
	if !signedIn || r.Header.Get("x-csrf-token") != csrfValue {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(Fixture(s.t, "forbidden.html"))
		return
	}
	if _, ok := s.filmByLID(lid); !ok {
		http.NotFound(w, r)
		return
	}
	if s.refused[lid] {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	body, _ := io.ReadAll(r.Body)
	raw, ok := strings.CutPrefix(string(body), `{"rating":`)
	raw, ok2 := strings.CutSuffix(raw, `}`)
	if !ok || !ok2 {
		s.t.Errorf("PATCH rate body = %q", body)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if raw == "null" {
		delete(s.ratings, lid)
		s.ratingOrder = without(s.ratingOrder, lid)
	} else {
		stars, err := strconv.ParseFloat(raw, 64)
		tenths := int(stars * 2)
		if err != nil || float64(tenths) != stars*2 || tenths < 1 || tenths > 10 {
			s.t.Errorf("PATCH rate body = %q", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.ratings[lid] = tenths
		s.ratingOrder = append([]string{lid}, without(s.ratingOrder, lid)...)
		s.watched[lid] = true
	}
	w.WriteHeader(http.StatusNoContent)
}

func without(lids []string, lid string) []string {
	kept := lids[:0:0]
	for _, existing := range lids {
		if existing != lid {
			kept = append(kept, existing)
		}
	}
	return kept
}

func (s *Site) filmByLID(lid string) (Film, bool) {
	for _, f := range s.films {
		if f.LID == lid {
			return f, true
		}
	}
	return Film{}, false
}

// writePage wraps a body in the site's page template. Like the real site, the
// person script always describes a guest: pages are cached templates and the
// signed-in member is filled in by JavaScript, so nothing may rely on it.
func (s *Site) writePage(w http.ResponseWriter, _ bool, title, body string) {
	w.Header().Set("content-type", "text/html; charset=UTF-8")
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><title>%s</title><script>
			person = {
				username: ""
				, loggedIn: false
				
				, showAds: true
				, role: "guest"
			};
</script><script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script></head>%s</html>`,
		html.EscapeString(title), body)
}

func displayName(f Film) string {
	if f.Year > 0 {
		return fmt.Sprintf("%s (%d)", f.Title, f.Year)
	}
	return f.Title
}

func posterDiv(f Film) string {
	name := html.EscapeString(displayName(f))
	ident := html.EscapeString(fmt.Sprintf(`{"lid":"%s","uid":"%s","type":"film","typeName":"film"}`, f.LID, f.UID))
	return fmt.Sprintf(`<div class="react-component" data-component-class="LazyPoster" data-item-name="%s" data-item-slug="%s" data-item-link="/film/%s/" data-item-full-display-name="%s" data-postered-identifier='%s' data-target-link="/film/%s/"><div class="poster film-poster"><img src="x.png" alt="%s"/></div></div>`,
		name, f.Slug, f.Slug, name, ident, f.Slug, html.EscapeString(f.Title))
}

// SolverURL starts a fake FlareSolverr service on first use and returns its
// base URL. Like the real one, it loads a page in a signed-out browser that
// gets past the challenge, follows redirects, and answers the page's status,
// final URL, and markup. Its requests are recorded as "SOLVE GET path".
func (s *Site) SolverURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.solver == nil {
		s.solver = httptest.NewServer(http.HandlerFunc(s.solve))
		s.t.Cleanup(s.solver.Close)
	}
	return s.solver.URL
}

// FailSolver makes the fake FlareSolverr answer with an error status and
// message (or stop, msg="").
func (s *Site) FailSolver(msg string) { s.mu.Lock(); s.solverError = msg; s.mu.Unlock() }

func (s *Site) solve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cmd        string `json:"cmd"`
		URL        string `json:"url"`
		MaxTimeout int    `json:"maxTimeout"`
		Cookies    []any  `json:"cookies"`
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1" || json.NewDecoder(r.Body).Decode(&req) != nil {
		s.t.Errorf("fake FlareSolverr: unexpected %s %s", r.Method, r.URL.Path)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Cmd != "request.get" || req.MaxTimeout <= 0 || len(req.Cookies) > 0 || !strings.HasPrefix(req.URL, s.URL+"/") {
		s.t.Errorf("fake FlareSolverr: unexpected request %+v", req)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.solverError != "" {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": s.solverError})
		return
	}
	target := req.URL
	var rec *httptest.ResponseRecorder
	for range 10 {
		page, _ := http.NewRequest(http.MethodGet, target, nil)
		s.requests = append(s.requests, "SOLVE GET "+page.URL.Path)
		rec = httptest.NewRecorder()
		s.route(rec, page)
		if rec.Code < 300 || rec.Code >= 400 {
			break
		}
		next, err := page.URL.Parse(rec.Header().Get("Location"))
		if err != nil {
			break
		}
		target = next.String()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"message": "Challenge not detected!",
		"solution": map[string]any{
			"url":      target,
			"status":   rec.Code,
			"response": rec.Body.String(),
		},
	})
}
