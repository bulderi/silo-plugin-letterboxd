// Package provider implements Silo's watch_sync_provider.v1 capability for a
// Letterboxd watchlist and ratings: movies only, both directions.
package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

const (
	itemKeyPrefix = "letterboxd:"

	// challengeBackoff is how long the host pauses a connection after
	// Cloudflare shows a challenge.
	challengeBackoff = time.Hour
	// deadlineHeadroom leaves time to answer before the host's RPC deadline.
	deadlineHeadroom = 30 * time.Second
)

// usernamePattern is what can be a Letterboxd username. It keeps an email
// address, a pasted URL, or a dot segment out of the watchlist path.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Options configures a Provider.
type Options struct {
	// BaseURL overrides https://letterboxd.com (tests).
	BaseURL string
	// Throttle spaces out every request the plugin makes.
	Throttle *letterboxd.Throttle
	// Store persists the film cache; nil keeps it in memory.
	Store StateStore
	// PageBudget is how long one ListRemoteState page may spend fetching film
	// pages before it hands what it has to the host.
	PageBudget time.Duration
	Logger     hclog.Logger
	Now        func() time.Time
	// Location dates diary entries; nil uses the process's time zone (TZ).
	Location *time.Location
	// FlareSolverrURL is a FlareSolverr service that loads challenged public
	// pages, for development on a server Cloudflare keeps challenging. Empty
	// in release builds.
	FlareSolverrURL string
}

// Provider is the WatchSyncProvider server.
type Provider struct {
	pluginv1.UnimplementedWatchSyncProviderServer

	baseURL    string
	solverURL  string
	throttle   *letterboxd.Throttle
	pageBudget time.Duration
	logger     hclog.Logger
	now        func() time.Time
	cache      *filmCache
	snapshots  *snapshotStore
	// accounts is what the plugin keeps per Letterboxd account.
	accounts *accountStore
	// watchlists are each account's last full watchlist read.
	watchlists *watchlistStore
	location   *time.Location
}

// New creates a Provider.
func New(opts Options) *Provider {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	budget := opts.PageBudget
	if budget <= 0 {
		budget = 60 * time.Second
	}
	location := opts.Location
	if location == nil {
		location = time.Local
	}
	if opts.FlareSolverrURL != "" {
		logger.Info("letterboxd challenges go through FlareSolverr")
	}
	return &Provider{
		location:   location,
		accounts:   newAccountStore(opts.Store, now),
		watchlists: newWatchlistStore(opts.Store, now),
		baseURL:    opts.BaseURL,
		solverURL:  opts.FlareSolverrURL,
		throttle:   opts.Throttle,
		pageBudget: budget,
		logger:     logger,
		now:        now,
		cache:      newFilmCache(opts.Store, now),
		snapshots:  newSnapshotStore(30*time.Minute, now),
	}
}

func (p *Provider) newClient() (*letterboxd.Client, error) {
	return letterboxd.New(letterboxd.Options{
		BaseURL: p.baseURL, Throttle: p.throttle, FlareSolverrURL: p.solverURL,
		Solved: func(path string) { p.logger.Info("letterboxd page loaded through FlareSolverr", "path", path) },
	})
}

// ExchangeAPIKey connects an account: the connection config carries the
// username, the API-key field carries the browser's remember-me cookie. The
// site's sign-in page sits behind a Cloudflare challenge, so a new connection
// never goes through it.
func (p *Provider) ExchangeAPIKey(ctx context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	invalid := func(message string) (*pluginv1.WatchSyncCredentialResponse, error) {
		return &pluginv1.WatchSyncCredentialResponse{Fault: newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, message)}, nil
	}
	username := strings.TrimSpace(req.GetProviderConfig().GetValues()[ConnectionUsernameKey])
	if username == "" || strings.TrimSpace(req.GetApiKey()) == "" {
		return invalid("Enter your Letterboxd username and the value of your letterboxd.user cookie.")
	}
	if !usernamePattern.MatchString(username) {
		return invalid("Enter your Letterboxd username as it appears in your profile address, not your email address.")
	}
	cookie, ok := letterboxd.ParseRememberCookie(req.GetApiKey())
	if !ok {
		return invalid("That is not a cookie value. Paste only the value of the letterboxd.user cookie.")
	}
	creds, err := p.connectWithCookie(ctx, username, cookie)
	if err != nil {
		p.logger.Warn("letterboxd connection failed", "error", err)
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultFor(err)}, nil
	}
	p.logger.Info("letterboxd account connected", "username", creds.account.Username)
	return &pluginv1.WatchSyncCredentialResponse{Credentials: creds.proto(), Account: accountProto(creds.account.Username)}, nil
}

// connectWithCookie accepts a cookie only if the site says it is signed in as
// this username. Reads go to the typed username's list while writes go to the
// cookie owner's, so a mismatch would make films added in Silo look removed
// from the list that is read.
func (p *Provider) connectWithCookie(ctx context.Context, username, cookie string) (credentials, error) {
	client, err := p.newClient()
	if err != nil {
		return credentials{}, err
	}
	client.SetSession(letterboxd.Session{Remember: cookie})
	owner, err := client.IsSignedInAs(ctx, username)
	if err != nil {
		return credentials{}, err
	}
	if !owner {
		return credentials{}, errCookieNotOwner
	}
	session := client.Session()
	session.CSRF = "" // see session.updated
	return credentials{account: account{Username: username}, session: session}, nil
}

// RefreshCredentials signs in again with the stored password. A cookie
// connection has nothing to renew with and returns what it has; a dead cookie
// shows when a read or write is refused.
func (p *Provider) RefreshCredentials(ctx context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	stored, ok := credentialsFromContext(req.GetContext())
	if !ok {
		return &pluginv1.WatchSyncCredentialResponse{Fault: reconnectFault()}, nil
	}
	if !stored.account.canSignIn() {
		return &pluginv1.WatchSyncCredentialResponse{Credentials: stored.proto(), Account: accountProto(stored.account.Username)}, nil
	}
	creds, err := p.signIn(ctx, stored.account.Login, stored.account.Password)
	if err != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultFor(err)}, nil
	}
	if !strings.EqualFold(creds.account.Username, stored.account.Username) {
		return &pluginv1.WatchSyncCredentialResponse{Fault: reconnectFault()}, nil
	}
	return &pluginv1.WatchSyncCredentialResponse{Credentials: creds.proto(), Account: accountProto(creds.account.Username)}, nil
}

// GetAccount reports the connected member from the stored credentials.
func (p *Provider) GetAccount(_ context.Context, req *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	creds, ok := credentialsFromContext(req.GetContext())
	if !ok {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: reconnectFault()}, nil
	}
	return &pluginv1.WatchSyncGetAccountResponse{Account: accountProto(creds.account.Username)}, nil
}

// signIn logs in and takes the canonical username from the cookie the site
// sets at sign-in, so an email login still yields the username that watchlist
// URLs use. Pages cannot tell: they are cached guest templates.
func (p *Provider) signIn(ctx context.Context, login, password string) (credentials, error) {
	client, err := p.newClient()
	if err != nil {
		return credentials{}, err
	}
	if err := client.Login(ctx, login, password); err != nil {
		return credentials{}, err
	}
	username := client.SignedInAs()
	if username == "" && !strings.Contains(login, "@") {
		username = login
	}
	if username == "" {
		return credentials{}, fmt.Errorf("sign-in did not name the member: %w", letterboxd.ErrUnexpectedPage)
	}
	return credentials{
		account: account{Username: username, Login: login, Password: password},
		session: client.Session(),
	}, nil
}

// session is one RPC's signed-in client for a connection.
type session struct {
	client   *letterboxd.Client
	creds    credentials
	original letterboxd.Session
	relogged bool
	// refused records that the site refused a write on a cookie connection.
	refused bool
	// logPlays is the connection's SettingLogPlays.
	logPlays bool
}

func (p *Provider) openSession(authCtx *pluginv1.WatchSyncAuthenticatedContext) (*session, *pluginv1.WatchSyncFault) {
	creds, ok := credentialsFromContext(authCtx)
	if !ok {
		return nil, reconnectFault()
	}
	client, err := p.newClient()
	if err != nil {
		return nil, faultFor(err)
	}
	client.SetSession(creds.session)
	return &session{
		client: client, creds: creds, original: creds.session,
		// A host without connection settings sends none, which is the
		// default: off.
		logPlays: authCtx.GetConnectionSettings()[SettingLogPlays] == "true",
	}, nil
}

// relogin signs in again once per RPC, for an expired session. A cookie
// connection has no password, so its user has to connect again.
func (s *session) relogin(ctx context.Context) error {
	if !s.creds.account.canSignIn() {
		return errCookieInvalid
	}
	if s.relogged {
		return letterboxd.ErrSignedOut
	}
	s.relogged = true
	s.client.ResetSession()
	if err := s.client.Login(ctx, s.creds.account.Login, s.creds.account.Password); err != nil {
		return err
	}
	// The login may now belong to another member (an email moved to a new
	// account); never act for someone else.
	if who := s.client.SignedInAs(); who != "" && !strings.EqualFold(who, s.creds.account.Username) {
		return letterboxd.ErrSignedOut
	}
	return nil
}

// updated returns the credentials to persist when the site rotated the
// session cookie or the RPC signed in again; nil when nothing changed.
func (s *session) updated() *pluginv1.WatchSyncCredentials {
	current := s.client.Session()
	if !s.creds.account.canSignIn() {
		// A cookie connection cannot sign in again to replace a stale CSRF
		// token, and every write would look like a dead cookie. It stores none
		// and loads a fresh one before its first write of each call.
		current.CSRF = ""
	}
	if current == s.original || (current.Current == "" && current.Remember == "") {
		return nil
	}
	creds := s.creds
	creds.session = current
	return creds.proto()
}

// ListRemoteState returns the watchlist or the rated films as one complete
// snapshot; the watchlist comes newest first. Reading the pages and looking up
// the films both spend a page budget, so a large or uncached list spreads over
// several host pages.
func (p *Provider) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	kind, ok := listKind(req.GetStateKinds())
	if !ok {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			"Letterboxd sync provides the watchlist, ratings, and diary, one at a time.")}, nil
	}
	noun := kind.String()
	sess, fault := p.openSession(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	p.loadCache(ctx)
	deadline := p.pageDeadline(ctx, p.now())
	username := sess.creds.account.Username

	var snap *snapshot
	if token := strings.TrimSpace(req.GetPageToken()); token == "" {
		var err error
		if snap, err = p.snapshots.start(username, kind); err != nil {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultFor(err)}, nil
		}
		snap.plan = p.planRead(kind, req.GetCursor())
		if snap.plan.stop != nil {
			snap.reader.StopWhen(snap.plan.stop)
		}
	} else {
		var ok bool
		if snap, ok = p.snapshots.resume(token, username, kind); !ok {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
				"The Letterboxd "+noun+" read expired before Silo finished it; the next sync starts over.")}, nil
		}
	}
	fail := func(err error, what string) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
		p.flushCache(ctx)
		p.snapshots.drop(snap.id)
		p.logger.Warn("letterboxd "+what+" failed", "username", username, "error", err)
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultFor(err), UpdatedCredentials: sess.updated()}, nil
	}

	// Each call makes progress by at least one page or one film lookup.
	worked := false
	for !snap.pagesDone() {
		if worked && !p.now().Before(deadline) {
			break
		}
		if err := p.readListPage(ctx, sess, snap.reader); err != nil {
			return fail(err, noun+" read")
		}
		worked = true
		if kind == letterboxd.ListWatchlist && snap.reader.PagesRead() == 1 && !snap.reader.Done() {
			if films, ok := p.watchlists.unchanged(ctx, username, snap.reader.FirstPage(), snap.reader.Total()); ok {
				snap.films, snap.readAt = films, p.now()
				p.logger.Info("letterboxd watchlist unchanged; serving the last full read", "username", username, "films", len(films))
			}
		}
	}
	if snap.pagesDone() && snap.films == nil {
		films, err := snap.reader.Films()
		if err != nil {
			return fail(err, noun+" read")
		}
		snap.films = append(make([]letterboxd.Poster, 0, len(films)), films...)
		snap.readAt = p.now()
		if kind != letterboxd.ListWatchlist {
			snap.cursor = p.nextCursor(snap, films)
		}
		if kind == letterboxd.ListDiary {
			snap.films, snap.plays = aggregateDiary(films)
		}
		if kind == letterboxd.ListWatchlist {
			if err := p.watchlists.save(ctx, username, films, len(snap.reader.FirstPage()), snap.reader.Total()); err != nil {
				p.logger.Warn("letterboxd watchlist snapshot not saved; the next read loads every page", "error", err)
			}
		}
		p.logger.Info("letterboxd "+noun+" read", "username", username, "incremental", snap.plan.incremental,
			"films", len(films), "pages", snap.reader.PagesRead())
	}

	var items []*pluginv1.WatchSyncRemoteState
	warnings := pageWarnings{kind: kind}
	for snap.films != nil && snap.offset < len(snap.films) {
		poster := snap.films[snap.offset]
		film, cached := p.cache.byLIDFresh(poster.LID)
		if !cached {
			// Only network work spends the page budget.
			if worked && !p.now().Before(deadline) {
				break
			}
			var stale bool
			var err error
			film, stale, err = p.fetchPoster(ctx, sess, poster)
			if errors.Is(err, errFilmPageMissing) && (kind == letterboxd.ListDiary || snap.plan.incremental) {
				// A diary read, or a ratings read that stopped early, is not
				// a snapshot, so a film left out is only not imported this
				// time. A full ratings read still fails, since Silo would read
				// the film as unrated.
				warnings.missing = append(warnings.missing, poster.Name)
				snap.offset++
				worked = true
				continue
			}
			if err != nil {
				return fail(err, "film lookup "+poster.Slug)
			}
			if stale {
				warnings.stale = append(warnings.stale, poster.Name)
			}
			worked = true
		}
		switch {
		case film.isMovie() && kind == letterboxd.ListDiary:
			if day, ok := p.diaryDay(poster.WatchedOn); ok {
				items = append(items, watchedState(film, day, snap.plays[poster.LID]))
			}
		case film.isMovie():
			items = append(items, remoteState(film, kind, poster.Rating, snap.readAt))
		case film.TMDBType == "tv":
			warnings.tv = append(warnings.tv, poster.Name)
		default:
			warnings.unlinked = append(warnings.unlinked, poster.Name)
		}
		snap.offset++
	}
	p.flushCache(ctx)

	resp := &pluginv1.WatchSyncListRemoteStateResponse{
		Items: items,
		// The diary is history: a play missing from a read is not a removal,
		// and Silo never removes plays on import. A ratings read that stopped
		// early is not the whole list.
		CompleteSnapshot:   kind == letterboxd.ListWatchlist || (kind == letterboxd.ListRatings && !snap.plan.incremental),
		UpdatedCredentials: sess.updated(),
		Warnings:           warnings.list(),
	}
	if snap.finished() {
		p.snapshots.drop(snap.id)
		resp.NextCursor = snap.cursor
	} else {
		resp.NextPageToken = p.snapshots.nextToken(snap)
	}
	return resp, nil
}

// readListPage reads the next list page with the member's session, so private
// watchlists work too. A public list reads fine without a session; a private
// one answers 403 once the session has expired, so the first page signs in
// again once.
func (p *Provider) readListPage(ctx context.Context, sess *session, reader *letterboxd.ListReader) error {
	err := reader.ReadPage(ctx, sess.client)
	if errors.Is(err, letterboxd.ErrForbidden) && reader.PagesRead() == 0 && !sess.relogged {
		p.logger.Info("letterboxd "+reader.Kind().String()+" was refused; signing in again", "username", sess.creds.account.Username)
		if err := sess.relogin(ctx); err != nil {
			return err
		}
		err = reader.ReadPage(ctx, sess.client)
	}
	return err
}

// errFilmPageMissing is a listed film whose page answers 404. Leaving the film
// out of a complete snapshot would make Silo remove it, so the read fails
// instead, unless an older lookup of the film is cached.
var errFilmPageMissing = errors.New("a listed film's page was not found")

// fetchPoster reads a listed film's external ids from its film page and
// caches them. stale reports that the page was gone and an older lookup was
// used instead.
func (p *Provider) fetchPoster(ctx context.Context, sess *session, poster letterboxd.Poster) (film cachedFilm, stale bool, err error) {
	page, err := sess.client.Film(ctx, poster.Slug)
	if errors.Is(err, letterboxd.ErrNotFound) {
		if old, ok := p.cache.byLIDAny(poster.LID); ok {
			return old, true, nil
		}
		return cachedFilm{}, false, fmt.Errorf("film %s: %w", poster.Slug, errFilmPageMissing)
	}
	if err != nil {
		return cachedFilm{}, false, err
	}
	film = cachedFromPage(page, p.now())
	film.LID = poster.LID
	if film.UID == "" {
		film.UID = poster.UID
	}
	if film.Title == "" {
		film.Title, film.Year = letterboxd.TitleYear(poster.Name)
	}
	p.cache.put(film)
	return film, false, nil
}

// remoteState is a film of a watchlist or ratings traversal. A rating comes
// from the ratings page, which only lists films with one.
//
// A rating carries the time of the read as its rated_at. Letterboxd shows no
// rating times, and Letterboxd is the member's source of truth: when Silo and
// Letterboxd both changed a film's rating since they last agreed, Silo keeps
// the newer one and, without a time, its own. On a first sync every differing
// rating counts as changed on both sides, so Silo's ratings, for example ones
// imported from another tracker, would overwrite the member's Letterboxd
// ratings. With the read time, Letterboxd's wins such a conflict, while a
// rating changed only in Silo still reaches Letterboxd.
func remoteState(film cachedFilm, kind letterboxd.ListKind, rating int, readAt time.Time) *pluginv1.WatchSyncRemoteState {
	state := filmState(film)
	if kind == letterboxd.ListRatings {
		state.Rating = &pluginv1.WatchSyncRemoteRatingState{Rating: int32(rating), RatedAt: timestamppb.New(readAt)}
	} else {
		state.Watchlist = &pluginv1.WatchSyncRemoteListState{}
	}
	return state
}

// watchedState is a film of a diary read: its entry count and newest date.
func watchedState(film cachedFilm, day time.Time, plays int) *pluginv1.WatchSyncRemoteState {
	state := filmState(film)
	state.Watched = &pluginv1.WatchSyncRemoteWatchedState{PlayCount: int32(max(plays, 1)), LastWatchedAt: timestamppb.New(day)}
	return state
}

func filmState(film cachedFilm) *pluginv1.WatchSyncRemoteState {
	ids := map[string]string{}
	if film.TMDBID != "" {
		ids["tmdb"] = film.TMDBID
	}
	if film.IMDbID != "" {
		ids["imdb"] = film.IMDbID
	}
	return &pluginv1.WatchSyncRemoteState{
		ProviderItemKey: itemKeyPrefix + film.LID,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			Title:       film.Title,
			Year:        int32(film.Year),
			ExternalIds: ids,
		},
	}
}

// ApplyEvents changes the Letterboxd watchlist, ratings, and diary.
func (p *Provider) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	sess, fault := p.openSession(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
	}
	p.loadCache(ctx)
	results := make([]*pluginv1.WatchSyncApplyResult, 0, len(req.GetEvents()))
	var paused *pluginv1.WatchSyncFault
	for _, event := range req.GetEvents() {
		if paused == nil && p.callNearlySpent(ctx) {
			// A diary play can take several throttled requests; the rest
			// waits for the next call rather than failing the whole one.
			paused = newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
				"Silo's time for this call ran out before this change; it will be retried.")
		}
		if paused != nil {
			results = append(results, applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, paused))
			continue
		}
		result, responseFault := p.applyOne(ctx, sess, event)
		if responseFault != nil {
			p.logger.Warn("letterboxd write stopped", "username", sess.creds.account.Username,
				"event", event.GetEventId(), "fault", responseFault.GetCode().String(), "reason", responseFault.GetSafeMessage())
			p.flushCache(ctx)
			return &pluginv1.WatchSyncApplyEventsResponse{Fault: responseFault, UpdatedCredentials: sess.updated()}, nil
		}
		p.logger.Info("letterboxd event", "username", sess.creds.account.Username,
			"event", event.GetEventId(), "operation", event.GetOperation().String(),
			"key", event.GetProviderItemKey(), "status", result.GetStatus().String(),
			"reason", result.GetFault().GetSafeMessage())
		if result.GetStatus() == pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY &&
			result.GetFault().GetCode() == pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED {
			paused = result.GetFault()
		}
		results = append(results, result)
	}
	p.flushCache(ctx)
	if sess.refused && paused == nil {
		// Only a confirmed dead cookie fails the connection; if the check itself
		// fails, the refused films are simply retried.
		alive, err := sess.client.SessionCanWrite(ctx)
		if err != nil {
			p.logger.Warn("letterboxd cookie check failed", "error", err)
		} else if !alive {
			p.logger.Warn("letterboxd no longer accepts the cookie", "username", sess.creds.account.Username)
			return &pluginv1.WatchSyncApplyEventsResponse{Fault: faultFor(errCookieInvalid), UpdatedCredentials: sess.updated()}, nil
		}
	}
	return &pluginv1.WatchSyncApplyEventsResponse{Results: results, UpdatedCredentials: sess.updated()}, nil
}

// applyOne handles one event. A non-nil second value fails the whole request,
// which is how a rejected password reaches the host.
func (p *Provider) applyOne(ctx context.Context, sess *session, event *pluginv1.WatchSyncEvent) (*pluginv1.WatchSyncApplyResult, *pluginv1.WatchSyncFault) {
	var write func(target writeTarget) error
	// ratingRemoval is a REMOVE_RATING, which must never be rejected: a film
	// Letterboxd does not have, or lists as TV, has no rating to remove.
	ratingRemoval := false
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST:
		write = func(target writeTarget) error { return sess.client.SetInWatchlist(ctx, target.lid, target.slug, true) }
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST:
		write = func(target writeTarget) error { return sess.client.SetInWatchlist(ctx, target.lid, target.slug, false) }
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING:
		rating := int(event.GetRating())
		if rating < 1 || rating > 10 {
			return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "A rating must be from 1 to 10."), nil
		}
		write = func(target writeTarget) error {
			// The entry first: an entry's rating also becomes the film's.
			if err := p.rateLoggedEntries(ctx, sess, target.lid, rating); err != nil {
				return err
			}
			return sess.client.SetRating(ctx, target.lid, target.slug, rating)
		}
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING:
		ratingRemoval = true
		write = func(target writeTarget) error {
			// The entry first, so no rated entry the plugin logged is left
			// to hold the film's rating up.
			if err := p.rateLoggedEntries(ctx, sess, target.lid, 0); err != nil {
				return err
			}
			return sess.client.SetRating(ctx, target.lid, target.slug, 0)
		}
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED:
		return p.applyWatched(ctx, sess, event)
	default:
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Letterboxd sync handles the watchlist, ratings, and diary."), nil
	}
	if event.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Letterboxd sync handles movies only."), nil
	}

	target, err := p.targetForEvent(ctx, sess, event)
	if err == nil && target.tv {
		if ratingRemoval {
			return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE, nil), nil
		}
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "Letterboxd lists this title as TV, not a movie."), nil
	}
	if err == nil {
		err = p.writeFilm(ctx, sess, func() error { return write(target) })
	}
	return p.writeResult(event, target, err, ratingRemoval)
}

// writeResult maps the outcome of a write to the event's result. A non-nil
// second value fails the whole request.
func (p *Provider) writeResult(event *pluginv1.WatchSyncEvent, target writeTarget, err error, ratingRemoval bool) (*pluginv1.WatchSyncApplyResult, *pluginv1.WatchSyncFault) {
	switch {
	case err == nil:
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED, nil), nil
	case errors.Is(err, letterboxd.ErrNotFound) && ratingRemoval:
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE, nil), nil
	case errors.Is(err, letterboxd.ErrNotFound):
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "This movie is not on Letterboxd."), nil
	case errors.Is(err, errWriteRefused):
		p.logger.Warn("letterboxd refused a change", "lid", target.lid, "operation", event.GetOperation().String())
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY,
			newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "Letterboxd refused this change; it will be retried.")), nil
	case errors.Is(err, letterboxd.ErrLoginRejected), errors.Is(err, letterboxd.ErrSignedOut):
		p.logger.Warn("letterboxd write lost its session", "error", err)
		return nil, faultFor(err)
	default:
		p.logger.Warn("letterboxd write failed", "error", err)
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, faultFor(err)), nil
	}
}

var (
	// errWriteRefused is a write the site refused while the account looks
	// fine, so only this change failed.
	errWriteRefused = errors.New("letterboxd refused the change")
	// errCookieInvalid is a cookie connection the site no longer accepts.
	errCookieInvalid = fmt.Errorf("letterboxd did not accept the cookie: %w", letterboxd.ErrSignedOut)
	// errCookieNotOwner is a new cookie that is dead or another member's.
	errCookieNotOwner = errors.New("the cookie is not signed in as this username")
)

// writeFilm sends one write for a film. A refusal with the stored session
// signs in again once per request; a refusal right after a successful sign-in
// is about this film, not the account. A cookie connection cannot sign in to
// tell the two apart, so ApplyEvents checks the cookie afterwards.
func (p *Provider) writeFilm(ctx context.Context, sess *session, write func() error) error {
	err := write()
	if !errors.Is(err, letterboxd.ErrSignedOut) {
		return err
	}
	if !sess.creds.account.canSignIn() {
		sess.refused = true
		return errWriteRefused
	}
	if !sess.relogged {
		p.logger.Info("letterboxd write was refused; signing in again", "username", sess.creds.account.Username)
		if err := sess.relogin(ctx); err != nil {
			return err
		}
		err = write()
		if !errors.Is(err, letterboxd.ErrSignedOut) {
			return err
		}
	}
	return errWriteRefused
}

// writeTarget is the Letterboxd film an event changes.
type writeTarget struct {
	lid  string
	uid  string // optional; the diary's rewatch check needs it
	slug string // optional; used as the Referer
	tv   bool   // Letterboxd maps the title to a TMDB TV entry
}

// targetForEvent finds the Letterboxd film for a Silo item: by the key a
// previous read returned, else by TMDB id, else by IMDb id.
func (p *Provider) targetForEvent(ctx context.Context, sess *session, event *pluginv1.WatchSyncEvent) (writeTarget, error) {
	if lid, ok := strings.CutPrefix(event.GetProviderItemKey(), itemKeyPrefix); ok && letterboxd.ValidLID(lid) {
		// Keys come from our own reads, which only return movies, so the LID
		// alone is enough to write.
		target := writeTarget{lid: lid}
		if film, ok := p.cache.byLIDFresh(lid); ok {
			target.slug, target.uid = film.Slug, film.UID
		}
		return target, nil
	}
	ids := event.GetMedia().GetExternalIds()
	lookups := []struct{ namespace, id string }{{"tmdb", ids["tmdb"]}, {"imdb", ids["imdb"]}}
	for _, lookup := range lookups {
		id := strings.TrimSpace(lookup.id)
		if id == "" {
			continue
		}
		film, ok := p.cache.byExternal(lookup.namespace, id)
		if !ok {
			page, err := sess.client.FilmByExternalID(ctx, lookup.namespace, id)
			if errors.Is(err, letterboxd.ErrNotFound) {
				continue
			}
			if err != nil {
				return writeTarget{}, err
			}
			film = cachedFromPage(page, p.now())
			p.cache.put(film)
		}
		// Letterboxd redirecting a TMDB id to a film with another TMDB id
		// means it is not this movie.
		if lookup.namespace == "tmdb" && film.TMDBID != "" && film.TMDBID != id {
			continue
		}
		return writeTarget{lid: film.LID, uid: film.UID, slug: film.Slug, tv: film.TMDBType == "tv"}, nil
	}
	return writeTarget{}, letterboxd.ErrNotFound
}

// callNearlySpent reports whether less than deadlineHeadroom is left before
// the host's deadline for this call, too little to start another change.
func (p *Provider) callNearlySpent(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < deadlineHeadroom
}

func (p *Provider) pageDeadline(ctx context.Context, started time.Time) time.Time {
	deadline := started.Add(p.pageBudget)
	if ctxDeadline, ok := ctx.Deadline(); ok {
		if limit := ctxDeadline.Add(-deadlineHeadroom); limit.Before(deadline) {
			deadline = limit
		}
	}
	return deadline
}

func (p *Provider) loadCache(ctx context.Context) {
	if err := p.cache.load(ctx); err != nil {
		p.logger.Warn("letterboxd film cache unavailable; continuing without it", "error", err)
	}
}

func (p *Provider) flushCache(ctx context.Context) {
	// The RPC context may be nearly spent; the write is small.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := p.cache.flush(flushCtx); err != nil {
		p.logger.Warn("letterboxd film cache write failed", "error", err)
	}
}

// listKind is the one list a ListRemoteState request asks for.
func listKind(kinds []pluginv1.WatchSyncRemoteStateKind) (letterboxd.ListKind, bool) {
	if len(kinds) != 1 {
		return 0, false
	}
	switch kinds[0] {
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST:
		return letterboxd.ListWatchlist, true
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING:
		return letterboxd.ListRatings, true
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED:
		return letterboxd.ListDiary, true
	default:
		return 0, false
	}
}

func applyResult(event *pluginv1.WatchSyncEvent, status pluginv1.WatchSyncApplyStatus, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncApplyResult {
	return &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: status, Fault: fault}
}

func rejected(event *pluginv1.WatchSyncEvent, code pluginv1.WatchSyncFaultCode, message string) *pluginv1.WatchSyncApplyResult {
	return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED, newFault(code, message))
}

func newFault(code pluginv1.WatchSyncFaultCode, message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{Code: code, SafeMessage: message}
}

func reconnectFault() *pluginv1.WatchSyncFault {
	return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
		"The Letterboxd connection is missing its sign-in details; connect Letterboxd again.")
}

// faultFor maps an error to a fault whose message is safe to show: it never
// includes page contents, cookies, or the password.
func faultFor(err error) *pluginv1.WatchSyncFault {
	var rateLimit *letterboxd.RateLimitError
	switch {
	case errors.Is(err, letterboxd.ErrChallenge):
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
			SafeMessage: "Letterboxd is showing a Cloudflare check; sync is paused for an hour.",
			RetryAfter:  durationpb.New(challengeBackoff),
		}
	case errors.As(err, &rateLimit):
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
			SafeMessage: "Letterboxd is rate limiting requests.",
			RetryAfter:  durationpb.New(rateLimit.RetryAfter),
		}
	case errors.Is(err, errCookieNotOwner):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			"That cookie is not signed in as this Letterboxd username. Check the username and copy the cookie again.")
	case errors.Is(err, errCookieInvalid):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			"Letterboxd does not accept this cookie; connect Letterboxd again with a new one.")
	case errors.Is(err, letterboxd.ErrLoginRejected):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			"Letterboxd rejected the username or password.")
	case errors.Is(err, letterboxd.ErrSignedOut):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			"Could not stay signed in to Letterboxd; connect Letterboxd again.")
	case errors.Is(err, letterboxd.ErrForbidden):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED,
			"Letterboxd denied access to the list.")
	case errors.Is(err, letterboxd.ErrNotFound):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			"The Letterboxd member or page was not found.")
	case errors.Is(err, errFilmPageMissing):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"A film on a Letterboxd list could not be read; the sync will be retried.")
	case errors.Is(err, errDiaryRead):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"The Letterboxd diary page for the play's date was not found; the play will be retried.")
	case errors.Is(err, letterboxd.ErrIncompleteList):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"The Letterboxd list changed while it was read; the sync will be retried.")
	case errors.Is(err, letterboxd.ErrUnexpectedPage):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"Letterboxd returned a page the plugin could not read; its layout may have changed.")
	default:
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"Letterboxd could not be reached; the sync will be retried.")
	}
}
