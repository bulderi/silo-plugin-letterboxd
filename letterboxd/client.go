// Package letterboxd talks to the Letterboxd website the way a signed-in
// browser does: HTML pages for reading, the site's own JSON endpoint for
// watchlist writes. Requests use a Chrome TLS and HTTP/2 fingerprint because
// the sign-in flow is behind Cloudflare.
package letterboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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

	csrfCookie       = "com.xk72.webparts.csrf"
	sessionCookie    = "letterboxd.user.CURRENT"
	rememberCookie   = "letterboxd.user"
	signedInAsCookie = "letterboxd.signed.in.as"

	maxBodyBytes = 8 << 20
)

var (
	// ErrChallenge means Cloudflare answered with a challenge page instead of
	// the site. The client never tries to solve one.
	ErrChallenge = errors.New("letterboxd answered with a Cloudflare challenge")
	// ErrNotFound means the page or film does not exist.
	ErrNotFound = errors.New("letterboxd page not found")
	// ErrForbidden is Letterboxd's own 403 page, e.g. a private watchlist.
	ErrForbidden = errors.New("letterboxd denied access to the page")
	// ErrSignedOut means the session is not (or no longer) signed in.
	ErrSignedOut = errors.New("letterboxd session is not signed in")
	// ErrLoginRejected means Letterboxd refused the username or password.
	ErrLoginRejected = errors.New("letterboxd rejected the sign-in")
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
}

// Session is the cookies that keep an account signed in and able to write.
type Session struct {
	// Current is the rotating session cookie (letterboxd.user.CURRENT).
	Current string
	// Remember is the long-lived remember-me cookie (letterboxd.user).
	Remember string
	// CSRF is the double-submit token cookie (com.xk72.webparts.csrf).
	// Keeping it saves a page load before each write.
	CSRF string
}

// Client is a single account's browser-like session. It is not safe for
// concurrent use.
type Client struct {
	base     *url.URL
	http     tls_client.HttpClient
	throttle *Throttle
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
	)
	if err != nil {
		return nil, fmt.Errorf("create http client: %w", err)
	}
	return &Client{base: base, http: hc, throttle: opts.Throttle}, nil
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
	if s.CSRF != "" {
		cookies = append(cookies, &http.Cookie{Name: csrfCookie, Value: s.CSRF, Path: "/"})
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
		CSRF:     c.cookie(csrfCookie),
	}
}

// ResetSession drops the session cookies, so a new sign-in starts as a guest
// rather than with a session the site may still partly honor.
func (c *Client) ResetSession() {
	var expired []*http.Cookie
	for _, name := range []string{sessionCookie, rememberCookie, signedInAsCookie, csrfCookie} {
		expired = append(expired, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
	c.http.SetCookies(c.rootURL(), expired)
}

// Login signs in with a username and password.
func (c *Client) Login(ctx context.Context, username, password string) error {
	if _, err := c.get(ctx, "/sign-in/", false); err != nil {
		return err
	}
	csrf := c.cookie(csrfCookie)
	if csrf == "" {
		return fmt.Errorf("sign-in page set no csrf cookie: %w", ErrUnexpectedPage)
	}
	form := url.Values{
		"__csrf":   {csrf},
		"username": {username},
		"password": {password},
		"remember": {"true"},
	}
	resp, err := c.send(ctx, request{
		method:  http.MethodPost,
		path:    "/user/login.do",
		body:    form.Encode(),
		referer: c.abs("/sign-in/"),
		headers: map[string]string{
			"content-type":     "application/x-www-form-urlencoded; charset=UTF-8",
			"accept":           "application/json, text/javascript, */*; q=0.01",
			"x-requested-with": "XMLHttpRequest",
		},
	})
	if err != nil {
		return err
	}
	if resp.status != http.StatusOK {
		return &StatusError{Method: http.MethodPost, Path: "/user/login.do", Status: resp.status}
	}
	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(resp.body, &result); err != nil {
		return fmt.Errorf("decode sign-in response: %w", ErrUnexpectedPage)
	}
	if result.Result != "success" {
		return ErrLoginRejected
	}
	if c.cookie(sessionCookie) == "" {
		return fmt.Errorf("sign-in set no session cookie: %w", ErrUnexpectedPage)
	}
	return nil
}

// SignedInAs is the canonical username the site set in the
// letterboxd.signed.in.as cookie at sign-in, or "" when there is none. It is
// only a hint for the UI: it can outlive the session, so it proves who signed
// in, not that the session is still valid.
func (c *Client) SignedInAs() string {
	value := c.cookie(signedInAsCookie)
	if unescaped, err := url.QueryUnescape(value); err == nil {
		value = unescaped
	}
	return strings.TrimSpace(value)
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

// WatchlistPath is the first watchlist page of a member.
func WatchlistPath(username string) string {
	return "/" + url.PathEscape(strings.ToLower(username)) + "/watchlist/"
}

// WatchlistPage loads one watchlist page: a path from WatchlistPath or a
// previous page's NextPath. A private watchlist read without a valid session
// is ErrForbidden.
func (c *Client) WatchlistPage(ctx context.Context, path string) (WatchlistPage, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "//") {
		return WatchlistPage{}, fmt.Errorf("watchlist path %q: %w", path, ErrUnexpectedPage)
	}
	resp, err := c.get(ctx, path, false)
	if err != nil {
		return WatchlistPage{}, err
	}
	page, err := ParseWatchlistPage(resp.body)
	if err != nil {
		return WatchlistPage{}, fmt.Errorf("watchlist %s: %w", path, err)
	}
	return page, nil
}

// Film loads a film page by slug.
func (c *Client) Film(ctx context.Context, slug string) (FilmPage, error) {
	resp, err := c.get(ctx, "/film/"+url.PathEscape(slug)+"/", false)
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
	resp, err := c.get(ctx, "/"+namespace+"/"+url.PathEscape(id)+"/", true)
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

// SetInWatchlist adds (in=true) or removes a film by LID. It sets the desired
// state, so repeating it is harmless.
func (c *Client) SetInWatchlist(ctx context.Context, lid, filmSlug string, in bool) error {
	if !ValidLID(lid) {
		return fmt.Errorf("invalid LID %q", lid)
	}
	if c.cookie(csrfCookie) == "" {
		if err := c.Home(ctx); err != nil {
			return err
		}
	}
	csrf := c.cookie(csrfCookie)
	if csrf == "" {
		return fmt.Errorf("no csrf cookie: %w", ErrUnexpectedPage)
	}
	referer := c.abs("/")
	if filmSlug != "" {
		referer = c.abs("/film/" + url.PathEscape(filmSlug) + "/")
	}
	path := "/api/v0/me/watchlist/" + lid
	resp, err := c.send(ctx, request{
		method:  http.MethodPatch,
		path:    path,
		body:    fmt.Sprintf(`{"inWatchlist":%t}`, in),
		referer: referer,
		headers: map[string]string{
			"content-type": "application/json; charset=UTF-8",
			"accept":       "*/*",
			"x-csrf-token": csrf,
		},
	})
	if err != nil {
		return err
	}
	switch resp.status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrSignedOut
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return &StatusError{Method: http.MethodPatch, Path: path, Status: resp.status}
	}
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
	defer resp.Body.Close()
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
	// Cloudflare injects /cdn-cgi/challenge-platform/ into every HTML page,
	// including Letterboxd's own 403 page, so only the interstitial's markers
	// count.
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
