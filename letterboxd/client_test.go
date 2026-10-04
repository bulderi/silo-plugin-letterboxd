package letterboxd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

var (
	quietHarbor = fakesite.Film{LID: "aA1", Slug: "the-quiet-harbor", Title: "The Quiet Harbor", Year: 2019,
		TMDBID: "5550001", TMDBType: "movie", IMDbID: "tt5550001"}
	northernLights = fakesite.Film{LID: "bB2", Slug: "northern-lights-2021", Title: "Northern Lights", Year: 2021,
		TMDBID: "5550002", TMDBType: "movie"}
	miniSeries = fakesite.Film{LID: "eE5", Slug: "the-mini-series", Title: "The Mini Series", Year: 2016,
		TMDBID: "5550005", TMDBType: "tv"}
)

func newSite(t *testing.T) (*fakesite.Site, *Client) {
	t.Helper()
	site := fakesite.New(t)
	site.AddFilms(quietHarbor, northernLights, miniSeries)
	client, err := New(Options{BaseURL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	return site, client
}

func TestUserAgentMatchesFake(t *testing.T) {
	if userAgent != fakesite.UserAgent {
		t.Fatal("fakesite.UserAgent must match the client's User-Agent")
	}
}

func TestLoginAndSession(t *testing.T) {
	site, client := newSite(t)
	ctx := context.Background()
	if err := client.Login(ctx, site.Username(), site.Password()); err != nil {
		t.Fatal(err)
	}
	if got := client.Session(); got.Current == "" || got.Remember == "" {
		t.Fatalf("session = %+v, want both cookies", got)
	}
	if got := client.SignedInAs(); got != site.Username() {
		t.Fatalf("signed in as %q, want %q", got, site.Username())
	}
	// Pages are guest templates even when signed in; the session shows in
	// what it may do.
	if err := client.SetInWatchlist(ctx, "aA1", "", true); err != nil {
		t.Fatalf("signed-in write: %v", err)
	}
}

func TestLoginWithEmail(t *testing.T) {
	site, client := newSite(t)
	if err := client.Login(context.Background(), site.Email(), site.Password()); err != nil {
		t.Fatal(err)
	}
	if got := client.SignedInAs(); got != site.Username() {
		t.Fatalf("signed in as %q, want the canonical username %q", got, site.Username())
	}
}

func TestLoginRejected(t *testing.T) {
	site, client := newSite(t)
	if err := client.Login(context.Background(), site.Username(), "wrong"); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("err = %v, want ErrLoginRejected", err)
	}
}

func TestLoginChallenged(t *testing.T) {
	site, client := newSite(t)
	site.Challenge("/sign-in/", true)
	if err := client.Login(context.Background(), site.Username(), site.Password()); !errors.Is(err, ErrChallenge) {
		t.Fatalf("err = %v, want ErrChallenge", err)
	}
}

func TestSessionRestoredFromCookies(t *testing.T) {
	site, first := newSite(t)
	site.SetPrivate(true)
	site.SetWatchlist("aA1")
	ctx := context.Background()
	if err := first.Login(ctx, site.Username(), site.Password()); err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{BaseURL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	second.SetSession(first.Session())
	if _, err := second.WatchlistPage(ctx, WatchlistPath(site.Username())); err != nil {
		t.Fatalf("restored session cannot read the private watchlist: %v", err)
	}
	site.ExpireSessions()
	if _, err := second.WatchlistPage(ctx, WatchlistPath(site.Username())); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expired session: err = %v, want ErrForbidden", err)
	}
}

func TestWatchlistPages(t *testing.T) {
	site, client := newSite(t)
	site.SetPageSize(1)
	site.SetWatchlist("aA1", "bB2")
	ctx := context.Background()
	page, err := client.WatchlistPage(ctx, WatchlistPath("SampleMember"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Films) != 1 || page.Films[0].LID != "aA1" || page.NextPath == "" {
		t.Fatalf("page 1 = %+v", page)
	}
	page, err = client.WatchlistPage(ctx, page.NextPath)
	if err != nil || len(page.Films) != 1 || page.Films[0].LID != "bB2" || page.NextPath != "" {
		t.Fatalf("page 2 = %+v, %v", page, err)
	}
}

func TestWatchlistErrors(t *testing.T) {
	site, client := newSite(t)
	site.SetWatchlist("aA1")
	ctx := context.Background()
	path := WatchlistPath(site.Username())

	site.SetPrivate(true)
	if _, err := client.WatchlistPage(ctx, path); !errors.Is(err, ErrForbidden) {
		t.Errorf("private: err = %v, want ErrForbidden", err)
	}
	site.SetPrivate(false)
	if _, err := client.WatchlistPage(ctx, WatchlistPath("nobody")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}
	site.Challenge(path, true)
	if _, err := client.WatchlistPage(ctx, path); !errors.Is(err, ErrChallenge) {
		t.Errorf("challenge: err = %v, want ErrChallenge", err)
	}
	site.Challenge(path, false)
	site.RateLimit(path, true)
	var rl *RateLimitError
	if _, err := client.WatchlistPage(ctx, path); !errors.As(err, &rl) || rl.RetryAfter != 2*time.Minute {
		t.Errorf("rate limit: err = %v", err)
	}
	if _, err := client.WatchlistPage(ctx, "https://example.com/x/"); !errors.Is(err, ErrUnexpectedPage) {
		t.Errorf("foreign path: err = %v", err)
	}
}

func TestFilmByExternalID(t *testing.T) {
	_, client := newSite(t)
	ctx := context.Background()
	film, err := client.FilmByExternalID(ctx, "tmdb", "5550001")
	if err != nil || film.LID != "aA1" || film.Slug != "the-quiet-harbor" || film.IMDbID != "tt5550001" {
		t.Fatalf("tmdb: %+v, %v", film, err)
	}
	film, err = client.FilmByExternalID(ctx, "imdb", "tt5550001")
	if err != nil || film.LID != "aA1" {
		t.Fatalf("imdb: %+v, %v", film, err)
	}
	for _, id := range []string{"404", "../sign-in", ""} {
		if _, err := client.FilmByExternalID(ctx, "tmdb", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("tmdb %q: err = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := client.FilmByExternalID(ctx, "tvdb", "1"); err == nil {
		t.Error("tvdb accepted")
	}
}

func TestSetInWatchlist(t *testing.T) {
	site, client := newSite(t)
	ctx := context.Background()
	if err := client.Login(ctx, site.Username(), site.Password()); err != nil {
		t.Fatal(err)
	}
	if err := client.SetInWatchlist(ctx, "aA1", "the-quiet-harbor", true); err != nil {
		t.Fatal(err)
	}
	if got := site.Watchlist(); len(got) != 1 || got[0] != "aA1" {
		t.Fatalf("watchlist = %v", got)
	}
	before := client.Session().Current
	site.RotateSessionOnWrite(true)
	if err := client.SetInWatchlist(ctx, "aA1", "", false); err != nil {
		t.Fatal(err)
	}
	if got := site.Watchlist(); len(got) != 0 {
		t.Fatalf("watchlist = %v, want empty", got)
	}
	if after := client.Session().Current; after == before || after == "" {
		t.Fatalf("session not rotated: before %q after %q", before, after)
	}
	// The rotated session keeps working.
	if err := client.SetInWatchlist(ctx, "bB2", "", true); err != nil {
		t.Fatal(err)
	}
}

func TestSetInWatchlistErrors(t *testing.T) {
	site, client := newSite(t)
	ctx := context.Background()
	if err := client.SetInWatchlist(ctx, "aA1", "", true); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("signed out: err = %v, want ErrSignedOut (Letterboxd's 403 is not a challenge)", err)
	}
	if err := client.Login(ctx, site.Username(), site.Password()); err != nil {
		t.Fatal(err)
	}
	if err := client.SetInWatchlist(ctx, "nOpe", "", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown film: err = %v, want ErrNotFound", err)
	}
	if err := client.SetInWatchlist(ctx, "../x", "", true); err == nil {
		t.Fatal("invalid LID accepted")
	}
}

func TestThrottleSpacesRequests(t *testing.T) {
	th := NewThrottle(40 * time.Millisecond)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := th.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("3 requests took %s, want at least 80ms", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewThrottle(time.Hour).Wait(ctx); err == nil {
		t.Fatal("cancelled wait returned nil")
	}
}

func TestThrottleReleasesCancelledSlot(t *testing.T) {
	th := NewThrottle(time.Hour)
	if err := th.Wait(context.Background()); err != nil { // takes the free slot
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := th.Wait(ctx); err == nil { // waits for the next slot, then gives up
		t.Fatal("wait should time out")
	}
	th.mu.Lock()
	next := th.next
	th.mu.Unlock()
	if until := time.Until(next); until > time.Hour+time.Minute || until < 59*time.Minute {
		t.Fatalf("next slot in %s, want about one interval (the cancelled slot released)", until)
	}
}
