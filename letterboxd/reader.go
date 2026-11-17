package letterboxd

import (
	"context"
	"errors"
	"fmt"
)

// maxListPages bounds a read if pagination ever loops.
const maxListPages = 1000

// ErrIncompleteList means the pages read do not add up to the entry count the
// site reported, for example because the list changed mid-read.
var ErrIncompleteList = errors.New("letterboxd list pages do not add up to its entry count")

// ListKind is a member list the plugin reads.
type ListKind int

const (
	// ListWatchlist is the member's watchlist, newest first.
	ListWatchlist ListKind = iota
	// ListRatings is every film the member has rated, with the rating.
	ListRatings
)

// ListReader reads a member's list one page at a time, so a caller can spread
// a long read over several calls. It is not safe for concurrent use.
type ListReader struct {
	kind      ListKind
	next      string
	pages     int
	seenPaths map[string]bool
	seenFilms map[string]bool
	films     []Poster
	total     int
}

// NewWatchlistReader starts at the member's first watchlist page.
func NewWatchlistReader(username string) *ListReader {
	return NewListReader(ListWatchlist, username)
}

// NewListReader starts at the first page of the member's list of that kind.
func NewListReader(kind ListKind, username string) *ListReader {
	start := WatchlistPath(username)
	if kind == ListRatings {
		start = RatingsPath(username)
	}
	return &ListReader{
		kind:      kind,
		next:      start,
		seenPaths: map[string]bool{},
		seenFilms: map[string]bool{},
		total:     -1,
	}
}

// Kind is the list the reader reads.
func (r *ListReader) Kind() ListKind { return r.kind }

// Done reports whether every page has been read.
func (r *ListReader) Done() bool { return r.next == "" }

// PagesRead is the number of pages read so far.
func (r *ListReader) PagesRead() int { return r.pages }

// ReadPage reads the next page. On error the reader is unchanged, so the same
// page can be read again, for example after signing in.
func (r *ListReader) ReadPage(ctx context.Context, c *Client) error {
	if r.Done() {
		return nil
	}
	if r.pages >= maxListPages || r.seenPaths[r.next] {
		return errors.New("letterboxd list pagination did not end")
	}
	var page ListPage
	var err error
	if r.kind == ListRatings {
		page, err = c.RatingsPage(ctx, r.next)
	} else {
		page, err = c.WatchlistPage(ctx, r.next)
	}
	if err != nil {
		return err
	}
	if r.pages == 0 {
		r.total = page.Total
	}
	r.seenPaths[r.next] = true
	r.pages++
	for _, film := range page.Films {
		if !r.seenFilms[film.LID] {
			r.seenFilms[film.LID] = true
			r.films = append(r.films, film)
		}
	}
	r.next = page.NextPath
	return nil
}

// Films returns the whole list, in the site's order, once every page is read.
// It fails when the films read do not match the entry count the first page
// reported, since a short list would look like removals.
func (r *ListReader) Films() ([]Poster, error) {
	if !r.Done() {
		return nil, errors.New("letterboxd list read is not finished")
	}
	if r.total >= 0 && len(r.films) != r.total {
		return nil, fmt.Errorf("read %d films, site reports %d: %w", len(r.films), r.total, ErrIncompleteList)
	}
	return r.films, nil
}
