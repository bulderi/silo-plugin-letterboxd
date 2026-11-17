// Command lbcheck checks, against the live site and with the plugin's own
// client, that this machine can use a Letterboxd cookie to read the watchlist
// ratings, and diary, and optionally add and remove one film, rate and unrate
// it, or log it in the diary. Run it on the Silo host when the
// sync misbehaves. With LETTERBOXD_PASSWORD instead of the cookie it tries the
// password sign-in that connections from before version 0.4.0 use.
//
// It never prints the password or cookie values. With -write it refuses a
// film that is already on the watchlist and removes the film it added. With
// -rate it refuses a film that is already rated, rates it, reads the rating
// back, and removes it. Letterboxd marks a rated film watched and keeps it
// watched after the rating is removed, so pick a film you have seen. With -log
// it refuses a film that is rated or already has a diary entry today, logs it
// for today, rates the entry, reads it back, and deletes the entry and the
// rating again.
//
//	read -rs LETTERBOXD_COOKIE; export LETTERBOXD_COOKIE
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -write -tmdb 11
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rate -tmdb 11
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rerate -tmdb 11
//	LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -log -tmdb 11
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
	rerate := flag.Bool("rerate", false, "change the rating of the already rated -tmdb film by half a star, check it becomes the most recently rated, and set the old rating back")
	logEntry := flag.Bool("log", false, "also log the -tmdb film in the diary for today, rate the entry, read it back, and delete it again")
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
	if (*write || *rate || *rerate || *logEntry) && *tmdbID <= 0 {
		fail("-write, -rate, -rerate, and -log need -tmdb")
	}
	if *rate && *rerate {
		fail("-rate needs a film that is not rated and -rerate one that is; run them separately")
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
		if !*write && !*rate && !*rerate && !*logEntry {
			fmt.Println("   only -write, -rate, -rerate, or -log proves the cookie works")
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
		debugPage(ctx, client, letterboxd.ListPath(letterboxd.ListWatchlist, username), username)
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

	step("read diary")
	entries, err := readList(ctx, client, letterboxd.ListDiary, username)
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("   %d diary entries\n", len(entries))
	if len(entries) > 0 {
		checkFilmRelation(ctx, client, entries[len(entries)-1], ratings)
	}
	if !*write && !*rate && !*rerate && !*logEntry {
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
	if _, rated := ratings[film.LID]; *rerate && !rated {
		fail("-rerate needs a film you have already rated")
	}
	if _, rated := ratings[film.LID]; *logEntry && rated {
		fail("-log rates the entry, which changes the film's rating; pick a film you have not rated")
	}
	if _, rated := ratings[film.LID]; *rate && rated {
		fail("this film is already rated; pick another so no rating of yours is removed")
	}
	if *write {
		checkWatchlistWrite(ctx, client, username, film, lids)
	}
	if *rerate {
		checkRerate(ctx, client, username, film, ratings[film.LID])
	}
	if *rate {
		checkRatingWrite(ctx, client, username, film, ratings)
	}
	if *logEntry {
		checkDiaryWrite(ctx, client, username, film)
	}
	fmt.Println("ok")
}

// checkDiaryWrite logs the film for today the way the plugin does, rates the
// entry 3.5 stars as a later Silo rating would, reads it back, and then
// deletes the entry and the film rating the entry gave, so nothing is left.
func checkDiaryWrite(ctx context.Context, client *letterboxd.Client, username string, film letterboxd.FilmPage) {
	step("log in the diary for today, rate the entry, and delete it")
	today := time.Now()
	logged, err := client.DiaryHasFilm(ctx, username, film.LID, today)
	if err != nil {
		fail("%v", err)
	}
	if logged {
		fail("this film already has a diary entry today; pick another")
	}
	relation, err := client.FilmRelation(ctx, film.UID)
	if err != nil {
		fail("rewatch check: %v", err)
	}
	rewatch := !relation.Earliest.IsZero()
	entry, err := client.LogFilm(ctx, film.LID, film.Slug, today, rewatch, relation.Rating)
	if err != nil {
		fail("log: %v", err)
	}
	if entry.ID == "" {
		fail("log: the site took the entry but its answer could not be read; delete it at %s%s",
			letterboxd.DefaultBaseURL, letterboxd.DiaryDayPath(username, today))
	}
	fmt.Printf("   logged entry %s (rewatch %v, rating %d/10)\n", entry.ID, entry.Rewatch, entry.Rating)
	cleanup := func() {
		if err := client.DeleteLogEntry(ctx, entry.ID); err != nil {
			fail("delete: %v (delete it at %s%s)", err, letterboxd.DefaultBaseURL, letterboxd.DiaryDayPath(username, today))
		}
		if err := client.SetRating(ctx, film.LID, film.Slug, 0); err != nil {
			fail("unrate: %v", err)
		}
	}
	if logged, err = client.DiaryHasFilm(ctx, username, film.LID, today); err != nil || !logged {
		cleanup()
		fail("logged, but the day's diary page does not show it (err %v)", err)
	}
	if _, err := client.SetLogEntryRating(ctx, entry, 7); err != nil {
		cleanup()
		fail("rate the entry: %v", err)
	}
	read, err := client.GetLogEntry(ctx, entry.ID)
	if err != nil || read.Rating != 7 || read.DiaryDate != entry.DiaryDate || read.Rewatch != entry.Rewatch {
		cleanup()
		fail("read back %+v (err %v), want rating 7 and the same date and rewatch flag", read, err)
	}
	fmt.Println("   rated the entry 7/10 and read it back")
	cleanup()
	if logged, err = client.DiaryHasFilm(ctx, username, film.LID, today); err != nil || logged {
		fail("deleted, but the day's diary page still shows it (err %v)", err)
	}
	fmt.Println("   deleted the entry and the film rating; the film stays marked watched")
}

// checkFilmRelation asks the Log form's question about the film of the
// oldest diary entry, without writing anything. Its earliest entry must exist
// and be no later than that entry, or the rewatch flag would be wrong, and its
// rating must match the ratings list, or new entries would get a wrong one.
func checkFilmRelation(ctx context.Context, client *letterboxd.Client, oldest letterboxd.Poster, ratings map[string]int) {
	step("rewatch check for " + oldest.Name)
	relation, err := client.FilmRelation(ctx, oldest.UID)
	if err != nil {
		fail("%v", err)
	}
	earliest := relation.Earliest
	// A derived rating reads as none, so only a rating the check reports must
	// match the list.
	if relation.Rating != 0 && relation.Rating != ratings[oldest.LID] {
		fail("the check gives rating %d, the ratings list %d", relation.Rating, ratings[oldest.LID])
	}
	entry, err := time.Parse("2006-01-02", oldest.WatchedOn)
	if err != nil {
		fail("diary entry date %q: %v", oldest.WatchedOn, err)
	}
	if earliest.IsZero() || earliest.After(entry) {
		fail("earliest entry %v, want one on or before the diary entry of %s", earliest, oldest.WatchedOn)
	}
	fmt.Printf("   earliest entry %s, diary entry %s: a new entry would be a rewatch with rating %d/10\n",
		earliest.Format("2006-01-02"), oldest.WatchedOn, relation.Rating)
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
// removes the rating. Each new rating must put the film first in the
// most-recently-rated order, which the plugin's short ratings reads rely on.
func checkRatingWrite(ctx context.Context, client *letterboxd.Client, username string, film letterboxd.FilmPage, before map[string]int) {
	step("rate, read back, and unrate")
	for _, rating := range []int{7, 8} {
		if err := client.SetRating(ctx, film.LID, film.Slug, rating); err != nil {
			fail("rate %d: %v (check %s/film/%s/)", rating, err, letterboxd.DefaultBaseURL, film.Slug)
		}
		// The most recently rated film is the first on the first page.
		page, err := client.ListPage(ctx, letterboxd.ListRatings, letterboxd.ListPath(letterboxd.ListRatings, username))
		if err != nil {
			fail("%v", err)
		}
		if len(page.Films) == 0 || page.Films[0].LID != film.LID {
			fail("rated %d, but the film is not the most recently rated; short ratings reads would miss the change", rating)
		}
		if page.Films[0].Rating != rating {
			fail("rated %d, read back %d", rating, page.Films[0].Rating)
		}
		fmt.Printf("   rated %d/10, read back first in the list\n", rating)
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

// checkRerate changes an older rating by half a star and checks the film
// becomes the most recently rated, which the plugin's short ratings reads rely
// on to see changed ratings. It then sets the old rating back; the film stays
// first in the most-recently-rated order.
func checkRerate(ctx context.Context, client *letterboxd.Client, username string, film letterboxd.FilmPage, old int) {
	step("change an older rating and set it back")
	page, err := client.ListPage(ctx, letterboxd.ListRatings, letterboxd.ListPath(letterboxd.ListRatings, username))
	if err != nil {
		fail("%v", err)
	}
	if len(page.Films) > 0 && page.Films[0].LID == film.LID {
		fail("this film is already the most recently rated, which proves nothing; pick an older rating")
	}
	changed := old + 1
	if old == 10 {
		changed = old - 1
	}
	for _, rating := range []int{changed, old} {
		if err := client.SetRating(ctx, film.LID, film.Slug, rating); err != nil {
			fail("rate %d: %v (your rating was %d/10: %s/film/%s/)", rating, err, old, letterboxd.DefaultBaseURL, film.Slug)
		}
		page, err := client.ListPage(ctx, letterboxd.ListRatings, letterboxd.ListPath(letterboxd.ListRatings, username))
		if err != nil {
			fail("%v (your rating was %d/10)", err, old)
		}
		if len(page.Films) == 0 || page.Films[0].LID != film.LID {
			if rating == changed {
				_ = client.SetRating(ctx, film.LID, film.Slug, old)
			}
			fail("rated %d, but the film did not become the most recently rated; short ratings reads would miss changed ratings until the daily full read", rating)
		}
		if page.Films[0].Rating != rating {
			fail("rated %d, read back %d (your rating was %d/10)", rating, page.Films[0].Rating, old)
		}
		fmt.Printf("   rated %d/10, read back first in the list\n", rating)
	}
	fmt.Printf("   your rating is %d/10 again\n", old)
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

// readList reads a whole list the way the plugin does.
func readList(ctx context.Context, client *letterboxd.Client, kind letterboxd.ListKind, username string) ([]letterboxd.Poster, error) {
	reader := letterboxd.NewListReader(kind, username)
	for !reader.Done() {
		if err := reader.ReadPage(ctx, client); err != nil {
			return nil, err
		}
	}
	return reader.Films()
}

func readWatchlist(ctx context.Context, client *letterboxd.Client, username string) (map[string]bool, error) {
	films, err := readList(ctx, client, letterboxd.ListWatchlist, username)
	lids := make(map[string]bool, len(films))
	for _, film := range films {
		lids[film.LID] = true
	}
	return lids, err
}

func readRatings(ctx context.Context, client *letterboxd.Client, username string) (map[string]int, error) {
	films, err := readList(ctx, client, letterboxd.ListRatings, username)
	ratings := make(map[string]int, len(films))
	for _, film := range films {
		ratings[film.LID] = film.Rating
	}
	return ratings, err
}
