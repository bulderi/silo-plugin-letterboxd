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
}

// WatchlistPage is one page of a member's watchlist.
type WatchlistPage struct {
	Films []Poster
	// NextPath is the path of the next (older) page, or "" on the last page.
	NextPath string
	// Total is the number of entries the whole watchlist has, from the
	// page's data-num-entries attribute, or -1 when the page does not say.
	Total int
}

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
	if !IsSitePage(body) {
		return WatchlistPage{}, ErrUnexpectedPage
	}
	page := WatchlistPage{Total: -1}
	z := html.NewTokenizer(bytes.NewReader(body))
	gridDepth := 0
	sawGrid := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if !sawGrid && page.Total != 0 {
				return WatchlistPage{}, ErrUnexpectedPage
			}
			return page, nil
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			attrs := attrMap(tok)
			switch {
			case tok.Data == "li" && hasClass(attrs["class"], "griditem"):
				gridDepth++
				sawGrid = true
			case gridDepth > 0 && attrs["data-component-class"] == "LazyPoster":
				poster, ok := posterFromAttrs(attrs)
				if !ok {
					return WatchlistPage{}, ErrUnexpectedPage
				}
				page.Films = append(page.Films, poster)
			case tok.Data == "a" && hasClass(attrs["class"], "next") && page.NextPath == "":
				page.NextPath = attrs["href"]
			case hasClass(attrs["class"], "js-watchlist-content") && page.Total < 0:
				if n, err := strconv.Atoi(strings.TrimSpace(attrs["data-num-entries"])); err == nil && n >= 0 {
					page.Total = n
				}
			}
		case html.EndTagToken:
			if tok := z.Token(); tok.Data == "li" && gridDepth > 0 {
				gridDepth--
			}
		}
	}
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
