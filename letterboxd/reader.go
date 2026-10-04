package letterboxd

import (
	"context"
	"errors"
	"fmt"
)

// maxWatchlistPages bounds a read if pagination ever loops.
const maxWatchlistPages = 1000

// ErrIncompleteWatchlist means the pages read do not add up to the entry count
// the site reported, for example because the watchlist changed mid-read.
var ErrIncompleteWatchlist = errors.New("letterboxd watchlist pages do not add up to its entry count")

// WatchlistReader reads a member's watchlist one page at a time, so a caller
// can spread a long read over several calls. It is not safe for concurrent
// use.
type WatchlistReader struct {
	next      string
	pages     int
	seenPaths map[string]bool
	seenFilms map[string]bool
	films     []Poster
	total     int
}

// NewWatchlistReader starts at the member's first watchlist page.
func NewWatchlistReader(username string) *WatchlistReader {
	return &WatchlistReader{
		next:      WatchlistPath(username),
		seenPaths: map[string]bool{},
		seenFilms: map[string]bool{},
		total:     -1,
	}
}

// Done reports whether every page has been read.
func (r *WatchlistReader) Done() bool { return r.next == "" }

// PagesRead is the number of pages read so far.
func (r *WatchlistReader) PagesRead() int { return r.pages }

// ReadPage reads the next page. On error the reader is unchanged, so the same
// page can be read again, for example after signing in.
func (r *WatchlistReader) ReadPage(ctx context.Context, c *Client) error {
	if r.Done() {
		return nil
	}
	if r.pages >= maxWatchlistPages || r.seenPaths[r.next] {
		return errors.New("letterboxd watchlist pagination did not end")
	}
	page, err := c.WatchlistPage(ctx, r.next)
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

// Films returns the whole watchlist, newest first, once every page is read.
// It fails when the films read do not match the entry count the first page
// reported, since a short list would look like removals.
func (r *WatchlistReader) Films() ([]Poster, error) {
	if !r.Done() {
		return nil, errors.New("letterboxd watchlist read is not finished")
	}
	if r.total >= 0 && len(r.films) != r.total {
		return nil, fmt.Errorf("read %d films, site reports %d: %w", len(r.films), r.total, ErrIncompleteWatchlist)
	}
	return r.films, nil
}
