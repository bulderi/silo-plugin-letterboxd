// Package letterboxd talks to the Letterboxd website the way a signed-in
// browser does: HTML pages for reading, the site's own JSON endpoints for
// watchlist, rating, and diary writes. Requests use a Chrome TLS and HTTP/2 fingerprint because
// the site sits behind Cloudflare.
package letterboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	DefaultBaseURL = "https://letterboxd.com"

	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	secChUA   = `"Chromium";v="152", "Google Chrome";v="152", "Not A(Brand";v="99"`

	maxRedirects   = 10
	csrfCookie     = "com.xk72.webparts.csrf"
	sessionCookie  = "letterboxd.user.CURRENT"
	rememberCookie = "letterboxd.user"

	maxBodyBytes = 8 << 20
)

var (
	// ErrChallenge means Cloudflare answered with a challenge page instead of
	// the site. The client tries to get past one only through a FlareSolverr
	// service, when Options sets one.
	ErrChallenge = errors.New("letterboxd answered with a Cloudflare challenge")
	// ErrNotFound means the page or film does not exist.
	ErrNotFound = errors.New("letterboxd page not found")
	// ErrForbidden is Letterboxd's own 403 page, e.g. a private watchlist.
	ErrForbidden = errors.New("letterboxd denied access to the page")
	// ErrSignedOut means the session is not (or no longer) signed in.
	ErrSignedOut = errors.New("letterboxd session is not signed in")
)

// RateLimitError is a 429 answer.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("letterboxd rate limit, retry after %s", e.RetryAfter)
}

// StatusError is any other unexpected status code.
type StatusError struct {
	Method string
	Path   string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("letterboxd %s %s returned %d", e.Method, e.Path, e.Status)
}

// Throttle spaces requests out. One Throttle is shared by every client in the
// process so several connected accounts do not multiply the request rate.
type Throttle struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

// NewThrottle allows at most one request per interval.
func NewThrottle(interval time.Duration) *Throttle { return &Throttle{interval: interval} }

// Wait blocks until the next request slot or until ctx ends.
func (t *Throttle) Wait(ctx context.Context) error {
	if t == nil || t.interval <= 0 {
		return ctx.Err()
	}
	t.mu.Lock()
	now := time.Now()
	slot := t.next
	if slot.Before(now) {
		slot = now
	}
	t.next = slot.Add(t.interval)
	t.mu.Unlock()
	wait := time.Until(slot)
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.release(slot)
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// release gives back a reserved slot whose request was never sent, when no
// later reservation depends on it.
func (t *Throttle) release(slot time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.next.Equal(slot.Add(t.interval)) {
		t.next = slot
	}
}

// Options configures a Client.
type Options struct {
	BaseURL  string
	Throttle *Throttle
	Timeout  time.Duration
	// FlareSolverr loads a challenged public page in a signed-out browser.
	// Nil means a challenge is an error.
	FlareSolverr *FlareSolverr
	// Solved, when set, is called with the path of each page loaded through
	// FlareSolverr.
	Solved func(path string)
}

// Session is the cookies that keep an account signed in and able to write.
type Session struct {
	// Current is the rotating session cookie (letterboxd.user.CURRENT).
	Current string
	// Remember is the long-lived remember-me cookie (letterboxd.user).
	Remember string
}

// Client is a single account's browser-like session. It is not safe for
// concurrent use.
type Client struct {
	base     *url.URL
	http     tls_client.HttpClient
	throttle *Throttle
	solver   *FlareSolverr
	solved   func(path string)
}

// New creates a client with an empty cookie jar.
func New(opts Options) (*Client, error) {
	raw := opts.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(profiles.Chrome_152),
		tls_client.WithTimeoutSeconds(int(timeout/time.Second)),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithCustomRedirectFunc(stayOnSite(base)),
	)
	if err != nil {
		return nil, fmt.Errorf("create http client: %w", err)
	}
	return &Client{base: base, http: hc, throttle: opts.Throttle, solver: opts.FlareSolverr, solved: opts.Solved}, nil
}

// ProbeLID is a film ID that cannot exist; see SessionCanWrite.
const ProbeLID = "zZzZzZzZzZzZzZzZ"

// stayOnSite refuses a redirect that leaves the base scheme and host. The
// client's cookie jar matches on the domain alone and ignores Secure, so this
// is what keeps the session cookies off plain HTTP.
func stayOnSite(base *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
			return errors.New("letterboxd redirected to another site")
		}
		return nil
	}
}

// ParseRememberCookie returns the remember-me cookie's value from what a member
// pasted, either the value alone or name=value. ok is false for anything that
// cannot be that cookie's value, such as a whole Cookie header or another
// Letterboxd cookie.
func ParseRememberCookie(input string) (value string, ok bool) {
	value = strings.TrimPrefix(strings.TrimSpace(input), rememberCookie+"=")
	if value == "" || strings.HasPrefix(value, "letterboxd.") {
		return "", false
	}
	for _, r := range value {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`";,\`, r) {
			return "", false
		}
	}
	return value, true
}

// SetSession puts the session cookies in the jar. Empty values are skipped.
func (c *Client) SetSession(s Session) {
	var cookies []*http.Cookie
	if s.Current != "" {
		cookies = append(cookies, &http.Cookie{Name: sessionCookie, Value: s.Current, Path: "/"})
	}
	if s.Remember != "" {
		cookies = append(cookies, &http.Cookie{Name: rememberCookie, Value: s.Remember, Path: "/"})
	}
	if len(cookies) > 0 {
		c.http.SetCookies(c.rootURL(), cookies)
	}
}

// Session returns the current session cookies, including any value the site
// rotated since SetSession.
func (c *Client) Session() Session {
	return Session{
		Current:  c.cookie(sessionCookie),
		Remember: c.cookie(rememberCookie),
	}
}

// IsSignedInAs reports whether the session belongs to username. Most pages are
// cached guest templates, but the settings page puts a signed-in member's
// username in its username field and has none for a guest.
func (c *Client) IsSignedInAs(ctx context.Context, username string) (bool, error) {
	resp, err := c.get(ctx, "/settings/", false)
	if err != nil {
		return false, err
	}
	owner := settingsUsername(resp.body)
	return owner != "" && strings.EqualFold(owner, username), nil
}

// SessionCanWrite reports whether the site accepts writes from this session,
// without changing anything: it asks that a film that cannot exist not be on
// the watchlist. The site answers 404 to a live session and 403 to a dead one.
func (c *Client) SessionCanWrite(ctx context.Context) (bool, error) {
	switch err := c.SetInWatchlist(ctx, ProbeLID, "", false); {
	case err == nil, errors.Is(err, ErrNotFound):
		return true, nil
	case errors.Is(err, ErrSignedOut):
		return false, nil
	default:
		return false, err
	}
}

// Home loads the front page, which makes sure the jar holds a CSRF cookie.
func (c *Client) Home(ctx context.Context) error {
	resp, err := c.get(ctx, "/", false)
	if err != nil {
		return err
	}
	if !IsSitePage(resp.body) {
		return fmt.Errorf("front page: %w", ErrUnexpectedPage)
	}
	return nil
}

// DebugPage fetches a page for diagnostics and reports its status, final path,
// body, and the names (not values) of the cookies in the jar.
func (c *Client) DebugPage(ctx context.Context, path string) (status int, finalPath string, body []byte, cookieNames []string, err error) {
	resp, err := c.send(ctx, request{method: http.MethodGet, path: path})
	if err != nil {
		return 0, "", nil, nil, err
	}
	for _, cookie := range c.http.GetCookies(c.rootURL()) {
		cookieNames = append(cookieNames, cookie.Name)
	}
	return resp.status, resp.path, resp.body, cookieNames, nil
}

// lists is where each member list starts, relative to the member's page, and
// how its pages read.
var lists = map[ListKind]struct {
	name  string
	path  string
	parse func([]byte) (ListPage, error)
}{
	ListWatchlist: {"watchlist", "/watchlist/", ParseWatchlistPage},
	// Most recently rated first, so a read can stop once it reaches films it
	// has seen.
	ListRatings: {"ratings", "/films/rated/.5-5/by/rated-date/", ParseRatingsPage},
	ListDiary:   {"diary", "/diary/films/", ParseDiaryPage},
}

// String is the list's name: "watchlist", "ratings", or "diary".
func (k ListKind) String() string {
	if list, ok := lists[k]; ok {
		return list.name
	}
	return "list " + strconv.Itoa(int(k))
}

func memberPath(username string) string {
	return "/" + url.PathEscape(strings.ToLower(username))
}

// ListPath is the first page of a member's list of that kind, or "" for a
// kind there is no such list for.
func ListPath(kind ListKind, username string) string {
	list, ok := lists[kind]
	if !ok {
		return ""
	}
	return memberPath(username) + list.path
}

// DiaryDayPath is the page of a member's diary entries for one date, which
// reads as a diary page.
func DiaryDayPath(username string, day time.Time) string {
	return memberPath(username) + day.Format("/diary/films/for/2006/01/02/")
}

// ListPage loads one page of a member's list: a path from ListPath, a
// previous page's NextPath, or for the diary DiaryDayPath. A private list
// read without a valid session is ErrForbidden. A challenged ratings or diary
// page is loaded through FlareSolverr when one is set; a watchlist page is
// not, because a private watchlist reads as refused signed out.
func (c *Client) ListPage(ctx context.Context, kind ListKind, path string) (ListPage, error) {
	return c.listPage(ctx, kind, path, kind != ListWatchlist)
}

func (c *Client) listPage(ctx context.Context, kind ListKind, path string, solvable bool) (ListPage, error) {
	list, ok := lists[kind]
	if !ok {
		return ListPage{}, fmt.Errorf("%s: %w", kind, ErrUnexpectedPage)
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "//") {
		return ListPage{}, fmt.Errorf("%s path %q: %w", list.name, path, ErrUnexpectedPage)
	}
	get := c.get
	if solvable {
		get = c.getPage
	}
	resp, err := get(ctx, path, false)
	if err != nil {
		return ListPage{}, err
	}
	page, err := list.parse(resp.body)
	if err != nil {
		return ListPage{}, fmt.Errorf("%s %s: %w", list.name, path, err)
	}
	return page, nil
}

// Film loads a film page by slug.
func (c *Client) Film(ctx context.Context, slug string) (FilmPage, error) {
	resp, err := c.getPage(ctx, "/film/"+url.PathEscape(slug)+"/", false)
	if err != nil {
		return FilmPage{}, err
	}
	film, err := ParseFilmPage(resp.body)
	if err != nil {
		return FilmPage{}, fmt.Errorf("film %s: %w", slug, err)
	}
	return film, nil
}

// FilmByExternalID follows Letterboxd's /tmdb/<id>/ or /imdb/<id>/ redirect to
// the film page. namespace is "tmdb" or "imdb".
func (c *Client) FilmByExternalID(ctx context.Context, namespace, id string) (FilmPage, error) {
	if namespace != "tmdb" && namespace != "imdb" {
		return FilmPage{}, fmt.Errorf("unsupported id namespace %q", namespace)
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsAny(id, "/?#") {
		return FilmPage{}, ErrNotFound
	}
	resp, err := c.getPage(ctx, "/"+namespace+"/"+url.PathEscape(id)+"/", true)
	if err != nil {
		return FilmPage{}, err
	}
	if !strings.Contains(resp.path, "/film/") {
		return FilmPage{}, ErrNotFound
	}
	film, err := ParseFilmPage(resp.body)
	if err != nil {
		return FilmPage{}, fmt.Errorf("%s %s: %w", namespace, id, err)
	}
	return film, nil
}

// DiaryHasFilm reports whether the member's diary has an entry for the film
// on that date. It never goes through FlareSolverr: signed out, a private
// entry could be missing, and the play would be logged twice.
func (c *Client) DiaryHasFilm(ctx context.Context, username, lid string, day time.Time) (bool, error) {
	page, err := c.listPage(ctx, ListDiary, DiaryDayPath(username, day), false)
	if err != nil {
		return false, err
	}
	for _, entry := range page.Films {
		if entry.LID == lid {
			return true, nil
		}
	}
	return false, nil
}

// filmUIDPattern is a film's site-wide id, as poster tiles carry it.
var filmUIDPattern = regexp.MustCompile(`^film:[A-Za-z0-9]{1,20}$`)

// Relation is what the site's Log form learns about a member and a film
// before it shows the form.
type Relation struct {
	// Earliest is the date of the member's earliest diary entry for the film,
	// or the zero time when there is none. The form ticks "rewatch" when it
	// is on or before the new entry's date.
	Earliest time.Time
	// Rating is the member's own rating of the film from 1 to 10, or 0 for
	// none. The form fills it in as the new entry's rating. A rating the site
	// derived rather than the member set (isDerivedRating) counts as none, so
	// logging never turns it into one the member gave.
	Rating int
}

// FilmRelation asks the endpoint the site's Log form asks, by the film's UID.
func (c *Client) FilmRelation(ctx context.Context, uid string) (Relation, error) {
	if !filmUIDPattern.MatchString(uid) {
		return Relation{}, fmt.Errorf("film uid %q: %w", uid, ErrUnexpectedPage)
	}
	body, err := c.getJSON(ctx, "/s/check-viewingable-relation?viewingableUID="+url.QueryEscape(uid))
	if err != nil {
		return Relation{}, err
	}
	var raw struct {
		Result              bool     `json:"result"`
		Rating              *float64 `json:"rating"`
		Derived             bool     `json:"isDerivedRating"`
		EarliestViewingDate *string  `json:"earliestViewingDate"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || !raw.Result {
		return Relation{}, fmt.Errorf("film relation: %w", ErrUnexpectedPage)
	}
	var relation Relation
	if raw.Rating != nil {
		rating := int(math.Round(*raw.Rating))
		if rating < 0 || rating > 10 || float64(rating) != *raw.Rating {
			return Relation{}, fmt.Errorf("film relation rating: %w", ErrUnexpectedPage)
		}
		if !raw.Derived {
			relation.Rating = rating
		}
	}
	if raw.EarliestViewingDate != nil && *raw.EarliestViewingDate != "" {
		date := *raw.EarliestViewingDate
		if len(date) < 10 {
			return Relation{}, fmt.Errorf("film relation date: %w", ErrUnexpectedPage)
		}
		day, err := time.Parse("2006-01-02", date[:10])
		if err != nil {
			return Relation{}, fmt.Errorf("film relation date: %w", ErrUnexpectedPage)
		}
		relation.Earliest = day
	}
	return relation, nil
}

// LogEntry is a diary entry as the site's API returns it.
type LogEntry struct {
	// ID is the entry's LID, which the read, update, and delete calls take.
	ID string
	// FilmLID is the logged film's LID.
	FilmLID   string
	DiaryDate string // YYYY-MM-DD
	Rewatch   bool
	// Rating is the entry's rating from 1 to 10, or 0 for none.
	Rating int
	Like   bool
	// Tags and Review report whether the member added any.
	Tags   int
	Review bool
	// Updated is the entry's whenUpdated, which changes whenever the entry is
	// edited, on the site or through the API.
	Updated string
}

// Plain reports whether the entry holds nothing but its date, rewatch flag,
// and rating, which is all SetLogEntryRating keeps.
func (e LogEntry) Plain() bool { return !e.Like && e.Tags == 0 && !e.Review }

type logEntryJSON struct {
	ID         string `json:"id"`
	Production struct {
		ID string `json:"id"`
	} `json:"production"`
	DiaryDetails *struct {
		DiaryDate string `json:"diaryDate"`
		Rewatch   bool   `json:"rewatch"`
	} `json:"diaryDetails"`
	Tags2       []json.RawMessage `json:"tags2"`
	Review      json.RawMessage   `json:"review"`
	Rating      *float64          `json:"rating"`
	Like        bool              `json:"like"`
	WhenUpdated string            `json:"whenUpdated"`
}

func (j logEntryJSON) entry() (LogEntry, error) {
	if !ValidLID(j.ID) || j.DiaryDetails == nil || j.WhenUpdated == "" {
		return LogEntry{}, fmt.Errorf("log entry: %w", ErrUnexpectedPage)
	}
	entry := LogEntry{
		ID: j.ID, FilmLID: j.Production.ID,
		DiaryDate: j.DiaryDetails.DiaryDate, Rewatch: j.DiaryDetails.Rewatch,
		Like: j.Like, Tags: len(j.Tags2), Updated: j.WhenUpdated,
		Review: len(j.Review) > 0 && string(j.Review) != "null",
	}
	if j.Rating != nil {
		entry.Rating = int(math.Round(*j.Rating * 2))
		if entry.Rating < 0 || entry.Rating > 10 {
			return LogEntry{}, fmt.Errorf("log entry rating: %w", ErrUnexpectedPage)
		}
	}
	return entry, nil
}

// LogFilm adds a diary entry for a film on a date, with the rating (1 to 10,
// or 0 for none) and no review, tags, or like, as the site's own "Log" form
// does, and returns the new entry: the zero LogEntry, with no error, when the
// site took the entry but its answer cannot be read. A rating on an entry also
// becomes the film's rating. Unlike the other writes it is not desired state:
// each call adds an entry, so callers check DiaryHasFilm first.
func (c *Client) LogFilm(ctx context.Context, lid, filmSlug string, day time.Time, rewatch bool, rating int) (LogEntry, error) {
	if !ValidLID(lid) {
		return LogEntry{}, fmt.Errorf("invalid LID %q", lid)
	}
	value, err := ratingValue(rating)
	if err != nil {
		return LogEntry{}, err
	}
	ratingField := ""
	if rating > 0 {
		ratingField = `,"rating":` + value
	}
	body := fmt.Sprintf(`{"productionId":%q,"diaryDetails":{"diaryDate":%q,"rewatch":%t},"tags":[]%s,"like":false}`,
		lid, day.Format("2006-01-02"), rewatch, ratingField)
	raw, err := c.sendJSON(ctx, http.MethodPost, "/api/v0/production-log-entries", filmSlug, body)
	if err != nil {
		return LogEntry{}, err
	}
	entry, err := decodeLogEntry(raw)
	if err != nil {
		return LogEntry{}, nil
	}
	return entry, nil
}

// GetLogEntry reads a diary entry by its LID.
func (c *Client) GetLogEntry(ctx context.Context, id string) (LogEntry, error) {
	if !ValidLID(id) {
		return LogEntry{}, fmt.Errorf("invalid log entry id %q", id)
	}
	body, err := c.getJSON(ctx, "/api/v0/log-entry/"+id)
	if err != nil {
		return LogEntry{}, err
	}
	var entry logEntryJSON
	if err := json.Unmarshal(body, &entry); err != nil {
		return LogEntry{}, fmt.Errorf("log entry: %w", ErrUnexpectedPage)
	}
	return entry.entry()
}

// decodeLogEntry reads the entry from a create or update answer, which wraps
// it as {"logEntry": ...}.
func decodeLogEntry(raw []byte) (LogEntry, error) {
	var answer struct {
		LogEntry logEntryJSON `json:"logEntry"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return LogEntry{}, fmt.Errorf("log entry answer: %w", ErrUnexpectedPage)
	}
	return answer.LogEntry.entry()
}

// getJSON reads one of the site's JSON answers. A refusal is ErrForbidden,
// not ErrSignedOut: whether the session is dead is for a write and the cookie
// check to decide, not a read.
func (c *Client) getJSON(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.send(ctx, request{
		method: http.MethodGet,
		path:   path,
		headers: map[string]string{
			"accept":           "application/json, text/javascript, */*; q=0.01",
			"x-requested-with": "XMLHttpRequest",
		},
	})
	if err != nil {
		return nil, err
	}
	switch resp.status {
	case http.StatusOK:
		return resp.body, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrForbidden
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		path, _, _ = strings.Cut(path, "?")
		return nil, &StatusError{Method: http.MethodGet, Path: path, Status: resp.status}
	}
}

// SetLogEntryRating sets a plain entry's rating (1 to 10, or 0 to clear it),
// keeping its date and rewatch flag, and returns the updated entry. The update
// replaces the entry's review, tags, and like, so it refuses an entry that
// has any of them rather than erase them.
func (c *Client) SetLogEntryRating(ctx context.Context, entry LogEntry, rating int) (LogEntry, error) {
	if !ValidLID(entry.ID) {
		return LogEntry{}, fmt.Errorf("invalid log entry id %q", entry.ID)
	}
	if !entry.Plain() {
		return LogEntry{}, errors.New("the entry has a review, tags, or a like that an update would erase")
	}
	value, err := ratingValue(rating)
	if err != nil {
		return LogEntry{}, err
	}
	body := fmt.Sprintf(`{"diaryDetails":{"diaryDate":%q,"rewatch":%t},"review":null,"tags":[],"rating":%s,"like":false,"privacyPolicy":null}`,
		entry.DiaryDate, entry.Rewatch, value)
	raw, err := c.sendJSON(ctx, http.MethodPatch, "/api/v0/production-log-entry/"+entry.ID, "", body)
	if err != nil {
		return LogEntry{}, err
	}
	return decodeLogEntry(raw)
}

// DeleteLogEntry removes a diary entry. The plugin never removes entries;
// cmd/lbcheck uses it to remove the entry it logged.
func (c *Client) DeleteLogEntry(ctx context.Context, id string) error {
	if !ValidLID(id) {
		return fmt.Errorf("invalid log entry id %q", id)
	}
	_, err := c.sendJSON(ctx, http.MethodDelete, "/api/v0/production-log-entry/"+id, "", "")
	return err
}

// ratingValue is a rating from 1 to 10 as the site's JSON takes it, in half
// stars, or null for 0, which clears it.
func ratingValue(rating int) (string, error) {
	if rating < 0 || rating > 10 {
		return "", fmt.Errorf("rating %d is outside 0 to 10", rating)
	}
	if rating == 0 {
		return "null", nil
	}
	return strconv.FormatFloat(float64(rating)/2, 'f', -1, 64), nil
}

// SetInWatchlist adds (in=true) or removes a film by LID. It sets the desired
// state, so repeating it is harmless.
func (c *Client) SetInWatchlist(ctx context.Context, lid, filmSlug string, in bool) error {
	return c.patchMe(ctx, "/api/v0/me/watchlist/", lid, filmSlug, fmt.Sprintf(`{"inWatchlist":%t}`, in))
}

// SetRating sets the member's rating of a film by LID, from 1 to 10 in half
// stars, or clears it when rating is 0. It sets the desired state, so
// repeating it is harmless. Rating a film also marks it watched on Letterboxd.
func (c *Client) SetRating(ctx context.Context, lid, filmSlug string, rating int) error {
	value, err := ratingValue(rating)
	if err != nil {
		return err
	}
	return c.patchMe(ctx, "/api/v0/me/rate/", lid, filmSlug, `{"rating":`+value+`}`)
}

// patchMe sends one of the site's own desired-state writes for a film, as the
// film page's buttons do.
func (c *Client) patchMe(ctx context.Context, prefix, lid, filmSlug, body string) error {
	if !ValidLID(lid) {
		return fmt.Errorf("invalid LID %q", lid)
	}
	_, err := c.sendJSON(ctx, http.MethodPatch, prefix+lid, filmSlug, body)
	return err
}

// sendJSON sends a JSON write to the site's own API with the CSRF token and
// returns the answer's body.
func (c *Client) sendJSON(ctx context.Context, method, path, filmSlug, body string) ([]byte, error) {
	if c.cookie(csrfCookie) == "" {
		if err := c.loadCSRFCookie(ctx, filmSlug); err != nil {
			return nil, err
		}
	}
	csrf := c.cookie(csrfCookie)
	if csrf == "" {
		return nil, fmt.Errorf("no csrf cookie: %w", ErrUnexpectedPage)
	}
	referer := c.abs("/")
	if filmSlug != "" {
		referer = c.abs("/film/" + url.PathEscape(filmSlug) + "/")
	}
	headers := map[string]string{"accept": "*/*", "x-csrf-token": csrf}
	if body != "" {
		headers["content-type"] = "application/json; charset=UTF-8"
	}
	resp, err := c.send(ctx, request{method: method, path: path, body: body, referer: referer, headers: headers})
	if err != nil {
		return nil, err
	}
	switch resp.status {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return resp.body, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrSignedOut
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, &StatusError{Method: method, Path: path, Status: resp.status}
	}
}

// loadCSRFCookie loads a page so the jar holds a CSRF cookie: the film's own
// page when the write is about a film, because Cloudflare challenges the
// front page far more often.
func (c *Client) loadCSRFCookie(ctx context.Context, filmSlug string) error {
	if filmSlug == "" {
		return c.Home(ctx)
	}
	_, err := c.get(ctx, "/film/"+url.PathEscape(filmSlug)+"/", false)
	return err
}

type request struct {
	method  string
	path    string
	body    string
	referer string
	headers map[string]string
	follow  bool
}

type response struct {
	status int
	body   []byte
	// path is the final path after redirects.
	path string
}

// get fetches a page and maps non-200 answers to errors.
func (c *Client) get(ctx context.Context, path string, follow bool) (response, error) {
	resp, err := c.send(ctx, request{method: http.MethodGet, path: path, follow: follow})
	if err != nil {
		return response{}, err
	}
	return pageResult(path, resp)
}

// pageResult maps a page's non-200 answer to an error.
func pageResult(path string, resp response) (response, error) {
	switch resp.status {
	case http.StatusOK:
		return resp, nil
	case http.StatusNotFound:
		return response{}, ErrNotFound
	case http.StatusForbidden:
		return response{}, ErrForbidden
	default:
		return response{}, &StatusError{Method: http.MethodGet, Path: path, Status: resp.status}
	}
}

func (c *Client) send(ctx context.Context, r request) (response, error) {
	if err := c.throttle.Wait(ctx); err != nil {
		return response{}, err
	}
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.abs(r.path), body)
	if err != nil {
		return response{}, err
	}
	req.Header = http.Header{
		"user-agent":         {userAgent},
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		"accept-language":    {"en"},
		"sec-ch-ua":          {secChUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"macOS"`},
		http.HeaderOrderKey: {
			"content-length", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"x-csrf-token", "x-requested-with", "user-agent", "content-type", "accept",
			"origin", "referer", "accept-encoding", "accept-language", "cookie",
		},
	}
	for key, value := range r.headers {
		req.Header.Set(key, value)
	}
	if r.method != http.MethodGet {
		req.Header.Set("origin", c.base.String())
	}
	if r.referer != "" {
		req.Header.Set("referer", r.referer)
	}
	c.http.SetFollowRedirect(r.follow)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return response{}, ctxErr
		}
		return response{}, fmt.Errorf("letterboxd %s %s: %w", r.method, r.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return response{}, fmt.Errorf("read letterboxd %s %s: %w", r.method, r.path, err)
	}
	if isChallenge(resp, raw) {
		return response{}, ErrChallenge
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return response{}, &RateLimitError{RetryAfter: retryAfter(resp.Header.Get("retry-after"))}
	}
	final := r.path
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.Path
	}
	return response{status: resp.StatusCode, body: raw, path: final}, nil
}

func (c *Client) abs(path string) string { return c.base.String() + path }

func (c *Client) rootURL() *url.URL {
	u := *c.base
	u.Path = "/"
	return &u
}

func (c *Client) cookie(name string) string {
	for _, cookie := range c.http.GetCookies(c.rootURL()) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

func isChallenge(resp *http.Response, body []byte) bool {
	if resp.Header.Get("cf-mitigated") == "challenge" {
		return true
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	return challengeBody(body)
}

// challengeBody reports whether a page is Cloudflare's interstitial.
// Cloudflare injects /cdn-cgi/challenge-platform/ into every HTML page,
// including Letterboxd's own 403 page, so only the interstitial's markers
// count.
func challengeBody(body []byte) bool {
	text := string(body)
	return strings.Contains(text, "challenges.cloudflare.com") ||
		strings.Contains(text, "<title>Just a moment...</title>")
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return time.Minute
}
