package provider

import (
	"fmt"
	"strings"
)

// maxWarningBytes keeps each note under the host's 300-byte limit per page
// warning, so the host never cuts one off mid-title.
const maxWarningBytes = 280

// pageWarnings collects safe notes about watchlist entries a ListRemoteState
// page left out or served from older data. The host shows them with the sync
// run, so someone can see why a Letterboxd entry did not reach Silo.
type pageWarnings struct {
	tv       []string
	unlinked []string
	stale    []string
}

func (w *pageWarnings) list() []string {
	var out []string
	add := func(names []string, one, many string) {
		switch len(names) {
		case 0:
		case 1:
			out = append(out, summarizeNames(one, names))
		default:
			out = append(out, summarizeNames(fmt.Sprintf(many, len(names)), names))
		}
	}
	add(w.tv,
		"Skipped 1 Letterboxd watchlist entry listed as TV, not a movie",
		"Skipped %d Letterboxd watchlist entries listed as TV, not movies")
	add(w.unlinked,
		"Skipped 1 film that has no TMDB or IMDb link on Letterboxd",
		"Skipped %d films that have no TMDB or IMDb link on Letterboxd")
	add(w.stale,
		"Used saved IDs for 1 film whose Letterboxd page was not found",
		"Used saved IDs for %d films whose Letterboxd page was not found")
	return out
}

// summarizeNames appends as many names as fit, then how many were left out.
func summarizeNames(lead string, names []string) string {
	var b strings.Builder
	b.WriteString(lead)
	for i, name := range names {
		sep := ": "
		if i > 0 {
			sep = "; "
		}
		rest := len(names) - i
		more := fmt.Sprintf("; and %d more", rest)
		if b.Len()+len(sep)+len(name)+len(more) > maxWarningBytes {
			if i == 0 {
				more = fmt.Sprintf(": %d not listed", rest)
			}
			b.WriteString(more)
			return b.String()
		}
		b.WriteString(sep)
		b.WriteString(name)
	}
	return b.String()
}
