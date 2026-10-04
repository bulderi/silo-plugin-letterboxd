// Command lbcheck checks, against the live site and with the plugin's own
// client, that this machine can sign in to Letterboxd, read the watchlist, and
// optionally add and remove one film. Run it on the Silo host when the sync
// misbehaves.
//
// It never prints the password or cookie values. With -write it refuses a
// film that is already on the watchlist and removes the film it added.
//
//	LETTERBOXD_USERNAME=... LETTERBOXD_PASSWORD=... go run ./cmd/lbcheck
//	LETTERBOXD_USERNAME=... LETTERBOXD_PASSWORD=... go run ./cmd/lbcheck -write -tmdb 11
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
	tmdbID := flag.Int("tmdb", 0, "TMDB id of a movie that is NOT on the watchlist (with -write)")
	interval := flag.Duration("interval", 1500*time.Millisecond, "minimum time between requests")
	debug = flag.Bool("debug", false, "after sign-in, show the page's person script with values redacted")
	flag.Parse()

	login := strings.TrimSpace(os.Getenv("LETTERBOXD_USERNAME"))
	password := os.Getenv("LETTERBOXD_PASSWORD")
	if login == "" || password == "" {
		fail("set LETTERBOXD_USERNAME and LETTERBOXD_PASSWORD")
	}
	if *write && *tmdbID <= 0 {
		fail("-write needs -tmdb with a movie that is not on the watchlist")
	}

	ctx := context.Background()
	client, err := letterboxd.New(letterboxd.Options{Throttle: letterboxd.NewThrottle(*interval)})
	if err != nil {
		fail("%v", err)
	}

	step("sign in")
	if err := client.Login(ctx, login, password); err != nil {
		fail("%v", err)
	}
	if *debug {
		debugPage(ctx, client, "/", login)
		debugPage(ctx, client, letterboxd.WatchlistPath(login), login)
	}
	username := client.SignedInAs()
	if username == "" {
		fail("sign-in set no letterboxd.signed.in.as cookie")
	}
	fmt.Printf("   signed in as %s\n", username)

	step("read watchlist")
	lids, err := readWatchlist(ctx, client, username)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("   %d films\n", len(lids))
	if !*write {
		fmt.Println("ok")
		return
	}

	step("resolve TMDB " + strconv.Itoa(*tmdbID))
	film, err := client.FilmByExternalID(ctx, "tmdb", strconv.Itoa(*tmdbID))
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("   /film/%s/ -> LID %s (TMDB %s %s)\n", film.Slug, film.LID, film.TMDBType, film.TMDBID)
	if lids[film.LID] {
		fail("this film is already on the watchlist; pick another so nothing of yours is removed")
	}

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
	fmt.Println("ok")
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
