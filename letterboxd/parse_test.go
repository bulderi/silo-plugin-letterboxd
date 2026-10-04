package letterboxd

import (
	"errors"
	"testing"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

var fixture = fakesite.Fixture

func TestIsSitePage(t *testing.T) {
	for _, name := range []string{"home_signed_out.html", "watchlist_page1.html", "film_movie.html"} {
		if !IsSitePage(fixture(t, name)) {
			t.Errorf("%s: not recognized as a site page", name)
		}
	}
	for _, name := range []string{"challenge.html", "forbidden.html"} {
		if IsSitePage(fixture(t, name)) {
			t.Errorf("%s: recognized as a site page", name)
		}
	}
}

func TestParseWatchlistPage(t *testing.T) {
	page, err := ParseWatchlistPage(fixture(t, "watchlist_page1.html"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Poster{
		{LID: "aA1", UID: "film:1001", Slug: "the-quiet-harbor", Name: "The Quiet Harbor (2019)"},
		{LID: "bB2", UID: "film:1002", Slug: "northern-lights-2021", Name: "Northern Lights (2021)"},
		{LID: "cC3", UID: "film:1003", Slug: "a-long-winter", Name: "A Long Winter & Spring (1998)"},
	}
	if len(page.Films) != len(want) {
		t.Fatalf("films = %+v, want %+v (sidebar posters must be ignored)", page.Films, want)
	}
	for i := range want {
		if page.Films[i] != want[i] {
			t.Errorf("film %d = %+v, want %+v", i, page.Films[i], want[i])
		}
	}
	if page.NextPath != "/samplemember/watchlist/page/2/" {
		t.Errorf("next = %q", page.NextPath)
	}
}

func TestParseWatchlistLastPage(t *testing.T) {
	page, err := ParseWatchlistPage(fixture(t, "watchlist_page2.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Films) != 2 || page.NextPath != "" {
		t.Fatalf("page = %+v, want 2 films and no next page", page)
	}
}

// A page without a film grid must never read as an empty watchlist: that
// would remove every synced film from Silo.
func TestParseWatchlistPageWithoutGridFails(t *testing.T) {
	for _, name := range []string{"watchlist_nogrid.html", "challenge.html", "forbidden.html", "home_signed_out.html"} {
		if _, err := ParseWatchlistPage(fixture(t, name)); !errors.Is(err, ErrUnexpectedPage) {
			t.Errorf("%s: err = %v, want ErrUnexpectedPage", name, err)
		}
	}
}

func TestParseFilmPage(t *testing.T) {
	film, err := ParseFilmPage(fixture(t, "film_movie.html"))
	if err != nil {
		t.Fatal(err)
	}
	want := FilmPage{
		LID: "aA1", Slug: "the-quiet-harbor", Title: "The Quiet Harbor", Year: 2019,
		TMDBID: "5550001", TMDBType: "movie", IMDbID: "tt5550001",
	}
	if film != want {
		t.Fatalf("film = %+v, want %+v", film, want)
	}
	if !film.IsMovie() {
		t.Fatal("movie not recognized")
	}
}

func TestParseFilmPageTV(t *testing.T) {
	film, err := ParseFilmPage(fixture(t, "film_tv.html"))
	if err != nil {
		t.Fatal(err)
	}
	if film.IsMovie() || film.TMDBType != "tv" || film.LID != "eE5" {
		t.Fatalf("film = %+v, want a TV entry", film)
	}
}

func TestParseFilmPageWithoutLinks(t *testing.T) {
	film, err := ParseFilmPage(fixture(t, "film_nolinks.html"))
	if err != nil {
		t.Fatal(err)
	}
	if film.TMDBID != "" || film.IMDbID != "" || film.IsMovie() || film.LID != "fF6" {
		t.Fatalf("film = %+v, want no external ids", film)
	}
}

func TestParseFilmPageFallsBackToShortLink(t *testing.T) {
	body := []byte(`<html><head><meta property="og:url" content="https://letterboxd.com/film/x-film/"><meta property="og:title" content="X Film (2001)"></head>` +
		`<body data-tmdb-type="movie" data-tmdb-id="42"><input value="https://boxd.it/Q9z" readonly></body></html>`)
	film, err := ParseFilmPage(body)
	if err != nil {
		t.Fatal(err)
	}
	if film.LID != "Q9z" || film.Title != "X Film" || film.Year != 2001 {
		t.Fatalf("film = %+v", film)
	}
}

func TestParseFilmPageRejectsOtherPages(t *testing.T) {
	for _, name := range []string{"challenge.html", "home_signed_out.html", "watchlist_page1.html"} {
		if _, err := ParseFilmPage(fixture(t, name)); !errors.Is(err, ErrUnexpectedPage) {
			t.Errorf("%s: err = %v, want ErrUnexpectedPage", name, err)
		}
	}
}

func TestTitleYear(t *testing.T) {
	cases := map[string]struct {
		title string
		year  int
	}{
		"Parasite (2019)":       {"Parasite", 2019},
		"Title (With) (1999)":   {"Title (With)", 1999},
		"No Year":               {"No Year", 0},
		"  Spaced Out (2020)  ": {"Spaced Out", 2020},
	}
	for in, want := range cases {
		title, year := TitleYear(in)
		if title != want.title || year != want.year {
			t.Errorf("TitleYear(%q) = %q, %d", in, title, year)
		}
	}
}

func TestParseWatchlistCountAndEmptyPage(t *testing.T) {
	page, err := ParseWatchlistPage(fixture(t, "watchlist_page1.html"))
	if err != nil || page.Total != 5 {
		t.Fatalf("total = %d, %v; want 5", page.Total, err)
	}
	page, err = ParseWatchlistPage(fixture(t, "watchlist_empty.html"))
	if err != nil || page.Total != 0 || len(page.Films) != 0 || page.NextPath != "" {
		t.Fatalf("empty watchlist = %+v, %v", page, err)
	}
}
