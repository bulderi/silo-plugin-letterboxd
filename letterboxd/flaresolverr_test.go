package letterboxd

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

func newSolvingSite(t *testing.T) (*fakesite.Site, *Client, *[]string) {
	t.Helper()
	site := fakesite.New(t)
	site.AddFilms(quietHarbor, northernLights, miniSeries)
	var solved []string
	solver, err := NewFlareSolverr(site.SolverURL())
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{BaseURL: site.URL, FlareSolverr: solver,
		Solved: func(path string) { solved = append(solved, path) }})
	if err != nil {
		t.Fatal(err)
	}
	return site, client, &solved
}

// The day's diary page decides whether a play is logged, and signed out it
// could miss a private entry, so it never goes through FlareSolverr.
func TestChallengedDiaryDayStaysAChallenge(t *testing.T) {
	site, client, _ := newSolvingSite(t)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	site.Challenge(DiaryDayPath(site.Username(), day), true)
	if _, err := client.DiaryHasFilm(context.Background(), site.Username(), "aA1", day); !errors.Is(err, ErrChallenge) {
		t.Fatalf("err = %v, want ErrChallenge", err)
	}
	if n := site.CountRequests("SOLVE "); n != 0 {
		t.Errorf("FlareSolverr was asked %d times", n)
	}
}

// Film lookups go through FlareSolverr too, including the /tmdb/ redirect,
// which the browser follows.
func TestChallengedFilmPagesGoThroughFlareSolverr(t *testing.T) {
	site, client, _ := newSolvingSite(t)
	ctx := context.Background()
	site.Challenge("/film/the-quiet-harbor/", true)
	site.Challenge("/tmdb/5550002/", true)
	film, err := client.Film(ctx, "the-quiet-harbor")
	if err != nil || film.TMDBID != "5550001" {
		t.Fatalf("film = %+v, %v", film, err)
	}
	film, err = client.FilmByExternalID(ctx, "tmdb", "5550002")
	if err != nil || film.Slug != "northern-lights-2021" {
		t.Fatalf("by tmdb = %+v, %v", film, err)
	}
	reqs := site.Requests()
	if !slices.Contains(reqs, "SOLVE GET /tmdb/5550002/") || !slices.Contains(reqs, "SOLVE GET /film/northern-lights-2021/") {
		t.Errorf("requests = %v", reqs)
	}
	if _, err := client.FilmByExternalID(ctx, "tmdb", "9999999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown film: err = %v, want ErrNotFound", err)
	}
}

// When FlareSolverr fails, the challenge stands, so the caller backs off as
// it would without it.
func TestFailedFlareSolverrIsStillAChallenge(t *testing.T) {
	site, client, solved := newSolvingSite(t)
	site.SetRatings(fakesite.Rating{LID: "aA1", Rating: 7})
	path := ListPath(ListRatings, site.Username())
	site.Challenge(path, true)
	site.FailSolver("Error solving the challenge. Timeout after 60.0 seconds.")
	if _, err := client.ListPage(context.Background(), ListRatings, path); !errors.Is(err, ErrChallenge) {
		t.Fatalf("err = %v, want ErrChallenge", err)
	}
	if len(*solved) != 0 {
		t.Errorf("solved = %v", *solved)
	}
}
