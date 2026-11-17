package letterboxd

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Poster is one film tile (a LazyPoster component).
type Poster struct {
	LID  string // short id used by the web API and boxd.it links
	UID  string // e.g. "film:426406"
	Slug string
	// Name is the display name including the year, e.g. "Parasite (2019)".
	Name string
	// Rating is the list owner's rating from 1 to 10 (half stars), shown on a
	// ratings page, or 0 when the tile shows none.
	Rating int
}

// ListPage is one page of a member's film list: the watchlist or the rated
// films.
type ListPage struct {
	Films []Poster
	// NextPath is the path of the next page, or "" on the last page.
	NextPath string
	// Total is the number of entries the whole list has, as the page reports
	// it, or -1 when the page does not say.
	Total int
}

// WatchlistPage is one page of a member's watchlist.
type WatchlistPage = ListPage

// FilmPage holds the identifiers of a film's own page.
type FilmPage struct {
	LID      string
	Slug     string
	Title    string
	Year     int
	TMDBID   string
	TMDBType string // "movie" or "tv"; empty when Letterboxd has no TMDB link
	IMDbID   string
}

// IsMovie reports whether the film maps to a TMDB movie. Letterboxd lists
// some TV miniseries and specials as films; those carry TMDB type "tv".
func (f FilmPage) IsMovie() bool { return f.TMDBType == "movie" }

var (
	personPattern    = regexp.MustCompile(`person\s*=\s*\{\s*username:`)
	imdbPattern      = regexp.MustCompile(`imdb\.com/title/(tt\d+)`)
	shortLinkPattern = regexp.MustCompile(`https://boxd\.it/([A-Za-z0-9]+)"`)
	nameYearPattern  = regexp.MustCompile(`^(.*\S)\s+\((\d{4})\)$`)
	lidPattern       = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	ratedPattern     = regexp.MustCompile(`^rated-(\d+)$`)
	// ratedCountPattern reads "... watched 1,685 films rated between ..." from
	// a ratings page's heading. The heading names a guest's view of the
	// member, so it reads the same for the member signed in.
	ratedCountPattern = regexp.MustCompile(`\bwatched\s+([\d,]+)\s+films?\b`)
)

// ErrUnexpectedPage means a page did not have the structure the parser
// expects, which usually means Letterboxd changed its markup.
var ErrUnexpectedPage = errors.New("letterboxd page has an unexpected structure")

// ValidLID reports whether s looks like a Letterboxd LID.
func ValidLID(s string) bool { return len(s) <= 16 && lidPattern.MatchString(s) }

// IsSitePage reports whether body is a page of the Letterboxd site rather
// than an error or challenge page: every site page carries an inline
// `person = { username: ..., loggedIn: ... }` script.
//
// That script cannot tell who is signed in. Letterboxd serves the same cached
// guest template to everyone and fills in the member with JavaScript, so it
// reads `loggedIn: false` even for a signed-in session.
func IsSitePage(body []byte) bool { return personPattern.Match(body) }

// TitleYear splits a display name such as "Parasite (2019)".
func TitleYear(name string) (string, int) {
	name = strings.TrimSpace(name)
	if m := nameYearPattern.FindStringSubmatch(name); m != nil {
		year, _ := strconv.Atoi(m[2])
		return m[1], year
	}
	return name, 0
}

// ParseWatchlistPage reads the film grid, the "older" pagination link, and
// the watchlist's total entry count. Only posters inside grid items count, so
// sidebar or promo posters never leak into the watchlist.
//
// A page without grid items is an empty watchlist only when the page itself
// reports zero entries; otherwise it is ErrUnexpectedPage. Reading "no films"
// from a page that merely failed to parse would delete every synced item in
// Silo.
func ParseWatchlistPage(body []byte) (WatchlistPage, error) {
	return parseListPage(body, false)
}

// ParseRatingsPage reads one page of a member's rated films
// (/<member>/films/rated/.5-5/): the films with the member's rating, the next
// page link, and the number of rated films from the page heading. Every film
// on the page must show a rating, and a page without films is an empty list
// only when the page says the member has rated none.
func ParseRatingsPage(body []byte) (ListPage, error) {
	page, err := parseListPage(body, true)
	if err != nil {
		return ListPage{}, err
	}
	for _, film := range page.Films {
		if film.Rating < 1 || film.Rating > 10 {
			return ListPage{}, ErrUnexpectedPage
		}
	}
	return page, nil
}

func parseListPage(body []byte, ratings bool) (ListPage, error) {
	if !IsSitePage(body) {
		return ListPage{}, ErrUnexpectedPage
	}
	page := ListPage{Total: -1}
	z := html.NewTokenizer(bytes.NewReader(body))
	gridDepth := 0
	sawGrid := false
	// gridFilms is the number of films when the current grid item started,
	// so a rating is only ever given to that item's own poster.
	gridFilms := 0
	// inHeading is inside the ratings page's <p class="ui-block-heading">. A
	// <p> cannot hold another, so its end tag ends the heading, whatever void
	// elements such as <br> it contains.
	inHeading := false
	var heading strings.Builder
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if ratings && page.Total < 0 {
				if m := ratedCountPattern.FindStringSubmatch(heading.String()); m != nil {
					if n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", "")); err == nil {
						page.Total = n
					}
				}
			}
			if !sawGrid && page.Total != 0 {
				return ListPage{}, ErrUnexpectedPage
			}
			return page, nil
		case html.TextToken:
			if inHeading {
				heading.Write(z.Text())
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			attrs := attrMap(tok)
			switch {
			case tok.Data == "li" && hasClass(attrs["class"], "griditem"):
				gridDepth++
				sawGrid = true
				gridFilms = len(page.Films)
			case gridDepth > 0 && attrs["data-component-class"] == "LazyPoster":
				poster, ok := posterFromAttrs(attrs)
				if !ok {
					return ListPage{}, ErrUnexpectedPage
				}
				page.Films = append(page.Films, poster)
			case ratings && gridDepth > 0 && hasClass(attrs["class"], "rating"):
				if n, ok := ratedClass(attrs["class"]); ok && len(page.Films) == gridFilms+1 {
					page.Films[gridFilms].Rating = n
				}
			case tok.Data == "a" && hasClass(attrs["class"], "next") && page.NextPath == "":
				page.NextPath = attrs["href"]
			case !ratings && hasClass(attrs["class"], "js-watchlist-content") && page.Total < 0:
				if n, err := strconv.Atoi(strings.TrimSpace(attrs["data-num-entries"])); err == nil && n >= 0 {
					page.Total = n
				}
			case ratings && hasClass(attrs["class"], "filtered-message") && hasClass(attrs["class"], "empty-text"):
				page.Total = 0
			case ratings && gridDepth == 0 && tok.Data == "p" && hasClass(attrs["class"], "ui-block-heading") && tt == html.StartTagToken:
				inHeading = true
			}
		case html.EndTagToken:
			tok := z.Token()
			if tok.Data == "p" {
				inHeading = false
			}
			if tok.Data == "li" && gridDepth > 0 {
				gridDepth--
			}
		}
	}
}

// ratedClass reads the rating from a class list such as
// "rating -micro -darker rated-7".
func ratedClass(classAttr string) (int, bool) {
	for _, c := range strings.Fields(classAttr) {
		if m := ratedPattern.FindStringSubmatch(c); m != nil {
			n, err := strconv.Atoi(m[1])
			return n, err == nil
		}
	}
	return 0, false
}

// ParseFilmPage reads a film page's identifiers.
func ParseFilmPage(body []byte) (FilmPage, error) {
	var film FilmPage
	var posters []Poster
	z := html.NewTokenizer(bytes.NewReader(body))
loop:
	for {
		switch z.Next() {
		case html.ErrorToken:
			break loop
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			attrs := attrMap(tok)
			switch {
			case tok.Data == "body":
				film.TMDBID = strings.TrimSpace(attrs["data-tmdb-id"])
				film.TMDBType = strings.TrimSpace(attrs["data-tmdb-type"])
			case tok.Data == "meta" && attrs["property"] == "og:url":
				film.Slug = slugFromFilmURL(attrs["content"])
			case tok.Data == "meta" && attrs["property"] == "og:title" && film.Title == "":
				film.Title, film.Year = TitleYear(attrs["content"])
			case attrs["data-component-class"] == "LazyPoster":
				if poster, ok := posterFromAttrs(attrs); ok {
					posters = append(posters, poster)
				}
			}
		}
	}
	if film.Slug == "" {
		return FilmPage{}, ErrUnexpectedPage
	}
	// The page shows other films' posters too (similar films, lists), so the
	// film's own LID is the poster whose slug matches the page.
	for _, poster := range posters {
		if poster.Slug == film.Slug {
			film.LID = poster.LID
			if title, year := TitleYear(poster.Name); year > 0 {
				film.Title, film.Year = title, year
			}
			break
		}
	}
	if film.LID == "" {
		if m := shortLinkPattern.FindSubmatch(body); m != nil {
			film.LID = string(m[1])
		}
	}
	if film.LID == "" {
		return FilmPage{}, ErrUnexpectedPage
	}
	if m := imdbPattern.FindSubmatch(body); m != nil {
		film.IMDbID = string(m[1])
	}
	if _, err := strconv.Atoi(film.TMDBID); err != nil {
		film.TMDBID, film.TMDBType = "", ""
	}
	return film, nil
}

func posterFromAttrs(attrs map[string]string) (Poster, bool) {
	var ident struct {
		LID  string `json:"lid"`
		UID  string `json:"uid"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(attrs["data-postered-identifier"]), &ident); err != nil {
		return Poster{}, false
	}
	if ident.Type != "film" || !ValidLID(ident.LID) {
		return Poster{}, false
	}
	slug := strings.TrimSpace(attrs["data-item-slug"])
	if slug == "" {
		slug = slugFromFilmURL(attrs["data-item-link"])
	}
	if slug == "" {
		return Poster{}, false
	}
	name := attrs["data-item-full-display-name"]
	if name == "" {
		name = attrs["data-item-name"]
	}
	return Poster{LID: ident.LID, UID: ident.UID, Slug: slug, Name: html.UnescapeString(name)}, true
}

// slugFromFilmURL extracts "parasite-2019" from ".../film/parasite-2019/".
func slugFromFilmURL(raw string) string {
	_, rest, ok := strings.Cut(raw, "/film/")
	if !ok {
		return ""
	}
	slug, _, _ := strings.Cut(rest, "/")
	return strings.TrimSpace(slug)
}

func attrMap(tok html.Token) map[string]string {
	attrs := make(map[string]string, len(tok.Attr))
	for _, a := range tok.Attr {
		attrs[a.Key] = a.Val
	}
	return attrs
}

func hasClass(classAttr, class string) bool {
	for _, c := range strings.Fields(classAttr) {
		if c == class {
			return true
		}
	}
	return false
}
