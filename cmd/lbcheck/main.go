// Command lbcheck checks, against the live site and with the plugin's own
// client, that this machine can use a Letterboxd cookie to read the watchlist
// and ratings, and optionally add and remove one film or rate and unrate it. Run it on the Silo host when the
// sync misbehaves. With LETTERBOXD_PASSWORD instead of the cookie it tries the
// password sign-in that connections from before version 0.4.0 use.
//
// It never prints the password or cookie values. With -write it refuses a
// film that is already on the watchlist and removes the film it added. With
// -rate it refuses a film that is already rated, rates it, reads the rating
// back, and removes it. Letterboxd marks a rated film watched and keeps it
// watched after the rating is removed, so pick a film you have seen.
//
//	read -rs LETTERBOXD_COOKIE; export LETTERBOXD_COOKIE
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -write -tmdb 11
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rate -tmdb 11
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

func main() {
	write := flag.Bool("write", false, "also add the -tmdb film to the watchlist and remove it again")
	rate := flag.Bool("rate", false, "also rate the -tmdb film, read the rating back, and remove it (the film stays marked watched)")
	tmdbID := flag.Int("tmdb", 0, "TMDB id of a movie that is NOT on the watchlist (with -write) and NOT rated (with -rate)")
	interval := flag.Duration("interval", 1500*time.Millisecond, "minimum time between requests")
	debug = flag.Bool("debug", false, "after sign-in, show the page's person script with values redacted")
	flag.Parse()

	username := strings.TrimSpace(os.Getenv("LETTERBOXD_USERNAME"))
	password := os.Getenv("LETTERBOXD_PASSWORD")
	rawCookie := os.Getenv("LETTERBOXD_COOKIE")
	if username == "" || (password == "" && rawCookie == "") {
		fail("set LETTERBOXD_USERNAME and LETTERBOXD_COOKIE (or LETTERBOXD_PASSWORD)")
	}
	if (*write || *rate) && *tmdbID <= 0 {
		fail("-write and -rate need -tmdb with a movie that is not on the watchlist and not rated")
	}

	ctx := context.Background()
	client, err := letterboxd.New(letterboxd.Options{Throttle: letterboxd.NewThrottle(*interval)})
	if err != nil {
		fail("%v", err)
	}

	if rawCookie != "" {
		cookie, ok := letterboxd.ParseRememberCookie(rawCookie)
		if !ok {
			fail("LETTERBOXD_COOKIE is not the value of the letterboxd.user cookie")
		}
		step("load front page with the cookie")
		client.SetSession(letterboxd.Session{Remember: cookie})
		if err := client.Home(ctx); err != nil {
			fail("%v", err)
		}
		if !*write && !*rate {
			fmt.Println("   only -write or -rate proves the cookie works")
		}
	} else {
		step("sign in")
		if err := client.Login(ctx, username, password); err != nil {
			fail("%v", err)
		}
		if username = client.SignedInAs(); username == "" {
			fail("sign-in set no letterboxd.signed.in.as cookie")
		}
		fmt.Printf("   signed in as %s\n", username)
	}
	if *debug {
		debugPage(ctx, client, "/", username)
		debugPage(ctx, client, letterboxd.WatchlistPath(username), username)
	}

	step("read watchlist")
	lids, err := readWatchlist(ctx, client, username)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("   %d films\n", len(lids))

	step("read ratings")
	ratings, err := readRatings(ctx, client, username)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("   %d rated films\n", len(ratings))
	if !*write && !*rate {
		fmt.Println("ok")
		return
	}

	step("resolve TMDB " + strconv.Itoa(*tmdbID))
	film, err := client.FilmByExternalID(ctx, "tmdb", strconv.Itoa(*tmdbID))
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("   /film/%s/ -> LID %s (TMDB %s %s)\n", film.Slug, film.LID, film.TMDBType, film.TMDBID)
	if *write && lids[film.LID] {
		fail("this film is already on the watchlist; pick another so nothing of yours is removed")
	}
	if _, rated := ratings[film.LID]; *rate && rated {
		fail("this film is already rated; pick another so no rating of yours is removed")
	}
	if *write {
		checkWatchlistWrite(ctx, client, username, film, lids)
	}
	if *rate {
		checkRatingWrite(ctx, client, username, film, ratings)
	}
	fmt.Println("ok")
}

func checkWatchlistWrite(ctx context.Context, client *letterboxd.Client, username string, film letterboxd.FilmPage, lids map[string]bool) {
	step("add and remove")
	if err := client.SetInWatchlist(ctx, film.LID, film.Slug, true); err != nil {
		fail("add: %v", err)
	}
	fmt.Println("   added")
	if err := client.SetInWatchlist(ctx, film.LID, film.Slug, false); err != nil {
		fail("remove: %v (the film may still be on the watchlist: %s/film/%s/)", err, letterboxd.DefaultBaseURL, film.Slug)
	}
	fmt.Println("   removed")
	after, err := readWatchlist(ctx, client, username)
	if err != nil {
		fail("%v", err)
	}
	if after[film.LID] || len(after) != len(lids) {
		fail("watchlist changed: %d films before, %d after", len(lids), len(after))
	}
}

// checkRatingWrite rates the film 3.5 stars, then 4, reads each back, and
// removes the rating.
func checkRatingWrite(ctx context.Context, client *letterboxd.Client, username string, film letterboxd.FilmPage, before map[string]int) {
	step("rate, read back, and unrate")
	for _, rating := range []int{7, 8} {
		if err := client.SetRating(ctx, film.LID, film.Slug, rating); err != nil {
			fail("rate %d: %v (check %s/film/%s/)", rating, err, letterboxd.DefaultBaseURL, film.Slug)
		}
		after, err := readRatings(ctx, client, username)
		if err != nil {
			fail("%v", err)
		}
		if after[film.LID] != rating {
			fail("rated %d, read back %d", rating, after[film.LID])
		}
		fmt.Printf("   rated %d/10, read back\n", rating)
	}
	if err := client.SetRating(ctx, film.LID, film.Slug, 0); err != nil {
		fail("unrate: %v (the film may still be rated: %s/film/%s/)", err, letterboxd.DefaultBaseURL, film.Slug)
	}
	after, err := readRatings(ctx, client, username)
	if err != nil {
		fail("%v", err)
	}
	if _, rated := after[film.LID]; rated || len(after) != len(before) {
		fail("ratings changed: %d rated films before, %d after", len(before), len(after))
	}
	fmt.Println("   unrated")
}

func readRatings(ctx context.Context, client *letterboxd.Client, username string) (map[string]int, error) {
	reader := letterboxd.NewListReader(letterboxd.ListRatings, username)
	for !reader.Done() {
		if err := reader.ReadPage(ctx, client); err != nil {
			return nil, err
		}
	}
	films, err := reader.Films()
	if err != nil {
		return nil, err
	}
	ratings := make(map[string]int, len(films))
	for _, film := range films {
		ratings[film.LID] = film.Rating
	}
	return ratings, nil
}

func readWatchlist(ctx context.Context, client *letterboxd.Client, username string) (map[string]bool, error) {
	reader := letterboxd.NewWatchlistReader(username)
	for !reader.Done() {
		if err := reader.ReadPage(ctx, client); err != nil {
			return nil, err
		}
	}
	films, err := reader.Films()
	if err != nil {
		return nil, err
	}
	lids := make(map[string]bool, len(films))
	for _, film := range films {
		lids[film.LID] = true
	}
	return lids, nil
}

var (
	debug         *bool
	quotedValue   = regexp.MustCompile(`"[^"]*"`)
	tokenLikeWord = regexp.MustCompile(`\b[A-Za-z0-9_\-]{24,}\b`)
)

// debugPage prints the person script of a page with every string value and
// token-like word redacted, except the member's own login name.
func debugPage(ctx context.Context, client *letterboxd.Client, path, login string) {
	status, final, body, cookies, err := client.DebugPage(ctx, path)
	if err != nil {
		fmt.Printf("   debug %s: %v\n", path, err)
		return
	}
	fmt.Printf("   debug GET %s -> %d (final %s), %d bytes, cookies %v\n", path, status, final, len(body), cookies)
	block := findPersonBlock(body)
	if block == nil {
		fmt.Println("   debug: no person block found")
		return
	}
	redacted := quotedValue.ReplaceAllFunc(block, func(v []byte) []byte {
		if strings.EqualFold(string(v), `"`+login+`"`) {
			return v
		}
		return []byte(`"…"`)
	})
	redacted = tokenLikeWord.ReplaceAll(redacted, []byte("…"))
	fmt.Printf("   debug person block:\n%s\n", redacted)
}

// findPersonBlock returns the inline `person = { ... };` script, at most 3000
// bytes of it.
func findPersonBlock(body []byte) []byte {
	start := bytes.Index(body, []byte("person = {"))
	if start < 0 {
		return nil
	}
	rest := body[start:]
	if len(rest) > 3000 {
		rest = rest[:3000]
	}
	if end := bytes.Index(rest, []byte("};")); end >= 0 {
		rest = rest[:end+2]
	}
	return rest
}

func step(name string) { fmt.Println(name) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
