// Package fakesite is an in-memory stand-in for letterboxd.com used by tests.
// It serves the same markup the real site does for the pages the plugin
// reads, with invented films and members.
package fakesite

import (
	"embed"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
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
	signedInCookie = "letterboxd.signed.in.as"
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

	mu        sync.Mutex
	username  string
	email     string
	password  string
	films     map[string]Film // by slug
	watchlist []string        // LIDs, newest first
	pageSize  int
	private   bool
	sessions  map[string]bool
	sessionN  int
	rotate    bool
	challenge map[string]bool
	rateLimit map[string]bool
	refused   map[string]bool
	missing   map[string]bool
	reported  *int
	requests  []string
}

// New starts a fake site for member "samplemember" (email
// samplemember@example.com, password "correct horse").
func New(t testing.TB) *Site {
	t.Helper()
	s := &Site{
		t:         t,
		username:  "samplemember",
		email:     "samplemember@example.com",
		password:  "correct horse",
		films:     map[string]Film{},
		pageSize:  28,
		sessions:  map[string]bool{},
		challenge: map[string]bool{},
		rateLimit: map[string]bool{},
		refused:   map[string]bool{},
		missing:   map[string]bool{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL
	return s
}

// Username is the member's canonical username.
func (s *Site) Username() string { return s.username }

// Email is the member's email, which also signs in.
func (s *Site) Email() string { return s.email }

// Password is the member's password.
func (s *Site) Password() string { return s.password }

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

// SetPageSize sets films per watchlist page.
func (s *Site) SetPageSize(n int) { s.mu.Lock(); s.pageSize = n; s.mu.Unlock() }

// SetPrivate makes the watchlist visible only when signed in.
func (s *Site) SetPrivate(private bool) { s.mu.Lock(); s.private = private; s.mu.Unlock() }

// SetPassword changes the password (existing sessions stay valid).
func (s *Site) SetPassword(p string) { s.mu.Lock(); s.password = p; s.mu.Unlock() }

// RotateSessionOnWrite makes every watchlist write issue a new session cookie.
func (s *Site) RotateSessionOnWrite(on bool) { s.mu.Lock(); s.rotate = on; s.mu.Unlock() }

// ExpireSessions signs every session out.
func (s *Site) ExpireSessions() { s.mu.Lock(); s.sessions = map[string]bool{}; s.mu.Unlock() }

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
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrfValue, Path: "/"})
	signedIn := s.signedIn(r)
	path := r.URL.Path
	watchlistRoot := "/" + s.username + "/watchlist/"
	switch {
	case path == "/":
		s.writePage(w, signedIn, "Letterboxd", `<body class="home"></body>`)
	case path == "/sign-in/":
		_, _ = w.Write(Fixture(s.t, "signin.html"))
	case path == "/user/login.do" && r.Method == http.MethodPost:
		s.login(w, r)
	case path == watchlistRoot || strings.HasPrefix(path, watchlistRoot+"page/"):
		s.watchlistPage(w, path, watchlistRoot, signedIn)
	case strings.HasPrefix(path, "/film/"):
		s.filmPage(w, r, strings.Trim(strings.TrimPrefix(path, "/film/"), "/"), signedIn)
	case strings.HasPrefix(path, "/tmdb/") || strings.HasPrefix(path, "/imdb/"):
		s.externalRedirect(w, r, path)
	case strings.HasPrefix(path, "/api/v0/me/watchlist/") && r.Method == http.MethodPatch:
		s.patchWatchlist(w, r, strings.TrimPrefix(path, "/api/v0/me/watchlist/"), signedIn)
	default:
		http.NotFound(w, r)
	}
}

func (s *Site) signedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sessions[c.Value]
}

func (s *Site) newSession(w http.ResponseWriter) {
	s.sessionN++
	id := "session-" + strconv.Itoa(s.sessionN)
	s.sessions[id] = true
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/"})
}

func (s *Site) login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.PostForm.Get("__csrf") != csrfValue {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	user := r.PostForm.Get("username")
	if (user != s.username && user != s.email) || r.PostForm.Get("password") != s.password {
		_, _ = io.WriteString(w, `{"result":"error","messages":["Your credentials don't match."]}`)
		return
	}
	s.newSession(w)
	http.SetCookie(w, &http.Cookie{Name: rememberCookie, Value: "remember-" + strconv.Itoa(s.sessionN), Path: "/"})
	http.SetCookie(w, &http.Cookie{Name: signedInCookie, Value: s.username, Path: "/"})
	_, _ = io.WriteString(w, `{"result":"success","messages":[]}`)
}

func (s *Site) watchlistPage(w http.ResponseWriter, path, root string, signedIn bool) {
	if s.private && !signedIn {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(Fixture(s.t, "forbidden.html"))
		return
	}
	page := 1
	if rest := strings.TrimPrefix(path, root+"page/"); rest != path {
		n, err := strconv.Atoi(strings.Trim(rest, "/"))
		if err != nil || n < 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		page = n
	}
	start := (page - 1) * s.pageSize
	if start > 0 && start >= len(s.watchlist) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	end := min(start+s.pageSize, len(s.watchlist))
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
		c, _ := r.Cookie(sessionCookie)
		delete(s.sessions, c.Value)
		s.newSession(w)
	}
	w.Header().Set("content-type", "application/json;charset=UTF-8")
	_, _ = io.WriteString(w, `{}`)
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
