package letterboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// solverTimeout is how long FlareSolverr may spend on one page when the
// context sets no earlier deadline.
const solverTimeout = 60 * time.Second

// flareSolverr loads pages in a real browser through a FlareSolverr service
// (https://github.com/FlareSolverr/FlareSolverr). It is for development on a
// server whose IP address Cloudflare keeps challenging; release builds leave
// it unset. The browser is signed out: a request carries none of the member's
// cookies.
type flareSolverr struct {
	endpoint string
	http     *http.Client
}

func newFlareSolverr(raw string) (*flareSolverr, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("FlareSolverr URL %q is not an http(s) URL", raw)
	}
	return &flareSolverr{endpoint: u.String() + "/v1", http: &http.Client{}}, nil
}

type solverAnswer struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		URL      string `json:"url"`
		Status   int    `json:"status"`
		Response string `json:"response"`
	} `json:"solution"`
}

// get loads pageURL and returns the page's status, final URL, and markup.
func (f *flareSolverr) get(ctx context.Context, pageURL string) (int, *url.URL, []byte, error) {
	timeout := solverTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return 0, nil, nil, context.DeadlineExceeded
	}
	payload, err := json.Marshal(map[string]any{
		"cmd": "request.get", "url": pageURL, "maxTimeout": timeout.Milliseconds(),
	})
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, nil, ctxErr
		}
		return 0, nil, nil, fmt.Errorf("flaresolverr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The markup arrives escaped inside JSON, so allow for some growth.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*maxBodyBytes))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read flaresolverr answer: %w", err)
	}
	var answer solverAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return 0, nil, nil, fmt.Errorf("flaresolverr answered %d without JSON", resp.StatusCode)
	}
	if answer.Status != "ok" {
		return 0, nil, nil, fmt.Errorf("flaresolverr: %s", answer.Message)
	}
	final, err := url.Parse(answer.Solution.URL)
	if err != nil {
		return 0, nil, nil, errors.New("flaresolverr answered without a page URL")
	}
	return answer.Solution.Status, final, []byte(answer.Solution.Response), nil
}

// getPage is get for a page that reads the same signed out, such as a
// public list or a film page. When Cloudflare challenges the client and a
// FlareSolverr service is set, the page is loaded through it instead.
func (c *Client) getPage(ctx context.Context, path string, follow bool) (response, error) {
	resp, err := c.get(ctx, path, follow)
	if c.solver == nil || !errors.Is(err, ErrChallenge) {
		return resp, err
	}
	return c.solve(ctx, path, follow)
}

// solve loads a page through FlareSolverr and maps the answer the way get
// maps the client's own.
func (c *Client) solve(ctx context.Context, path string, follow bool) (response, error) {
	if err := c.throttle.Wait(ctx); err != nil {
		return response{}, err
	}
	status, final, body, err := c.solver.get(ctx, c.abs(path))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return response{}, ctxErr
		}
		// The challenge still stands, so the caller backs off as it would
		// without FlareSolverr.
		return response{}, fmt.Errorf("%w; FlareSolverr failed: %w", ErrChallenge, err)
	}
	// The browser follows redirects; the client follows them only when asked
	// and never off the site.
	if final.Scheme != c.base.Scheme || final.Host != c.base.Host {
		return response{}, errors.New("letterboxd redirected to another site")
	}
	if !follow && final.EscapedPath() != path && final.Path != path {
		return response{}, &StatusError{Method: http.MethodGet, Path: path, Status: http.StatusFound}
	}
	// A rendered page loads Cloudflare's scripts too, so as in isChallenge
	// the markers count only on a refusal.
	if (status == http.StatusForbidden || status == http.StatusServiceUnavailable) && challengeBody(body) {
		return response{}, ErrChallenge
	}
	if status == http.StatusTooManyRequests {
		return response{}, &RateLimitError{RetryAfter: retryAfter("")}
	}
	resp, err := pageResult(path, response{status: status, body: body, path: final.Path})
	if err == nil && c.solved != nil {
		c.solved(path)
	}
	return resp, err
}
