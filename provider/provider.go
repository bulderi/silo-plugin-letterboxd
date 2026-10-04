// Package provider implements Silo's watch_sync_provider.v1 capability for a
// Letterboxd watchlist: movies only, both directions.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/durationpb"

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
}

// Provider is the WatchSyncProvider server.
type Provider struct {
	pluginv1.UnimplementedWatchSyncProviderServer

	baseURL    string
	throttle   *letterboxd.Throttle
	pageBudget time.Duration
	logger     hclog.Logger
	now        func() time.Time
	cache      *filmCache
	snapshots  *snapshotStore
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
	return &Provider{
		baseURL:    opts.BaseURL,
		throttle:   opts.Throttle,
		pageBudget: budget,
		logger:     logger,
		now:        now,
		cache:      newFilmCache(opts.Store, now),
		snapshots:  newSnapshotStore(30*time.Minute, now),
	}
}

func (p *Provider) newClient() (*letterboxd.Client, error) {
	return letterboxd.New(letterboxd.Options{BaseURL: p.baseURL, Throttle: p.throttle})
}

// ExchangeAPIKey connects an account: the connection config carries the
// username or email, the API-key field carries the password.
func (p *Provider) ExchangeAPIKey(ctx context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	login := strings.TrimSpace(req.GetProviderConfig().GetValues()[ConnectionUsernameKey])
	password := req.GetApiKey()
	if login == "" || strings.TrimSpace(password) == "" {
		return &pluginv1.WatchSyncCredentialResponse{Fault: newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			"Enter your Letterboxd username and password.")}, nil
	}
	creds, err := p.signIn(ctx, login, password)
	if err != nil {
		p.logger.Warn("letterboxd sign-in failed", "error", err)
		return &pluginv1.WatchSyncCredentialResponse{Fault: faultFor(err)}, nil
	}
	p.logger.Info("letterboxd account connected", "username", creds.account.Username)
	return &pluginv1.WatchSyncCredentialResponse{Credentials: creds.proto(), Account: accountProto(creds.account.Username)}, nil
}

// RefreshCredentials signs in again with the stored password.
func (p *Provider) RefreshCredentials(ctx context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	stored, ok := credentialsFromContext(req.GetContext())
	if !ok {
		return &pluginv1.WatchSyncCredentialResponse{Fault: reconnectFault()}, nil
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
	return &session{client: client, creds: creds, original: creds.session}, nil
}

// relogin signs in again once per RPC, for an expired session.
func (s *session) relogin(ctx context.Context) error {
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
	if current == s.original || current.Current == "" {
		return nil
	}
	creds := s.creds
	creds.session = current
	return creds.proto()
}

// ListRemoteState returns the watchlist as one complete, ordered snapshot,
// newest first. Reading the pages and looking up the films both spend a page
// budget, so a large or uncached watchlist spreads over several host pages.
func (p *Provider) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	if !onlyWatchlist(req.GetStateKinds()) {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
			"Letterboxd sync only provides the watchlist.")}, nil
	}
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
		if snap, err = p.snapshots.start(username); err != nil {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: faultFor(err)}, nil
		}
	} else {
		var ok bool
		if snap, ok = p.snapshots.resume(token, username); !ok {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
				"The Letterboxd watchlist read expired before Silo finished it; the next sync starts over.")}, nil
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
		if err := p.readWatchlistPage(ctx, sess, snap.reader); err != nil {
			return fail(err, "watchlist read")
		}
		worked = true
	}
	if snap.pagesDone() && snap.films == nil {
		films, err := snap.reader.Films()
		if err != nil {
			return fail(err, "watchlist read")
		}
		snap.films = append(make([]letterboxd.Poster, 0, len(films)), films...)
		p.logger.Info("letterboxd watchlist read", "username", username,
			"films", len(films), "pages", snap.reader.PagesRead())
	}

	var items []*pluginv1.WatchSyncRemoteState
	for snap.films != nil && snap.offset < len(snap.films) {
		poster := snap.films[snap.offset]
		film, cached := p.cache.byLIDFresh(poster.LID)
		if !cached {
			// Only network work spends the page budget.
			if worked && !p.now().Before(deadline) {
				break
			}
			var err error
			if film, err = p.fetchPoster(ctx, sess, poster); err != nil {
				return fail(err, "film lookup "+poster.Slug)
			}
			worked = true
		}
		if film.isMovie() {
			items = append(items, remoteState(film))
		}
		snap.offset++
	}
	p.flushCache(ctx)

	resp := &pluginv1.WatchSyncListRemoteStateResponse{
		Items:              items,
		CompleteSnapshot:   true,
		UpdatedCredentials: sess.updated(),
	}
	if snap.finished() {
		p.snapshots.drop(snap.id)
	} else {
		resp.NextPageToken = p.snapshots.nextToken(snap)
	}
	return resp, nil
}

// readWatchlistPage reads the next watchlist page with the member's session,
// so private watchlists work too. A public watchlist reads fine without a
// session; a private one answers 403 once the session has expired, so the
// first page signs in again once.
func (p *Provider) readWatchlistPage(ctx context.Context, sess *session, reader *letterboxd.WatchlistReader) error {
	err := reader.ReadPage(ctx, sess.client)
	if errors.Is(err, letterboxd.ErrForbidden) && reader.PagesRead() == 0 && !sess.relogged {
		p.logger.Info("letterboxd watchlist was refused; signing in again", "username", sess.creds.account.Username)
		if err := sess.relogin(ctx); err != nil {
			return err
		}
		err = reader.ReadPage(ctx, sess.client)
	}
	return err
}

// errFilmPageMissing is a watchlisted film whose page answers 404. Leaving
// the film out of a complete snapshot would make Silo remove it, so the read
// fails instead, unless an older lookup of the film is cached.
var errFilmPageMissing = errors.New("a watchlisted film's page was not found")

// fetchPoster reads a watchlist film's external ids from its film page and
// caches them.
func (p *Provider) fetchPoster(ctx context.Context, sess *session, poster letterboxd.Poster) (cachedFilm, error) {
	page, err := sess.client.Film(ctx, poster.Slug)
	if errors.Is(err, letterboxd.ErrNotFound) {
		if stale, ok := p.cache.byLIDAny(poster.LID); ok {
			return stale, nil
		}
		return cachedFilm{}, fmt.Errorf("film %s: %w", poster.Slug, errFilmPageMissing)
	}
	if err != nil {
		return cachedFilm{}, err
	}
	film := cachedFromPage(page, p.now())
	film.LID = poster.LID
	if film.Title == "" {
		film.Title, film.Year = letterboxd.TitleYear(poster.Name)
	}
	p.cache.put(film)
	return film, nil
}

func remoteState(film cachedFilm) *pluginv1.WatchSyncRemoteState {
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
		Watchlist: &pluginv1.WatchSyncRemoteListState{},
	}
}

// ApplyEvents adds films to or removes them from the Letterboxd watchlist.
func (p *Provider) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	sess, fault := p.openSession(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
	}
	p.loadCache(ctx)
	results := make([]*pluginv1.WatchSyncApplyResult, 0, len(req.GetEvents()))
	var paused *pluginv1.WatchSyncFault
	for _, event := range req.GetEvents() {
		if paused != nil {
			results = append(results, applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, paused))
			continue
		}
		result, responseFault := p.applyOne(ctx, sess, event)
		if responseFault != nil {
			p.logger.Warn("letterboxd watchlist write stopped", "username", sess.creds.account.Username,
				"event", event.GetEventId(), "fault", responseFault.GetCode().String(), "reason", responseFault.GetSafeMessage())
			p.flushCache(ctx)
			return &pluginv1.WatchSyncApplyEventsResponse{Fault: responseFault, UpdatedCredentials: sess.updated()}, nil
		}
		p.logger.Info("letterboxd watchlist event", "username", sess.creds.account.Username,
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
	return &pluginv1.WatchSyncApplyEventsResponse{Results: results, UpdatedCredentials: sess.updated()}, nil
}

// applyOne handles one event. A non-nil second value fails the whole request,
// which is how a rejected password reaches the host.
func (p *Provider) applyOne(ctx context.Context, sess *session, event *pluginv1.WatchSyncEvent) (*pluginv1.WatchSyncApplyResult, *pluginv1.WatchSyncFault) {
	var in bool
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST:
		in = true
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST:
		in = false
	default:
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Letterboxd sync only handles the watchlist."), nil
	}
	if event.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Letterboxd sync handles movies only."), nil
	}

	target, err := p.targetForEvent(ctx, sess, event)
	if err == nil && target.tv {
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "Letterboxd lists this title as TV, not a movie."), nil
	}
	if err == nil {
		err = p.writeWatchlist(ctx, sess, target, in)
	}
	switch {
	case err == nil:
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED, nil), nil
	case errors.Is(err, letterboxd.ErrNotFound):
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "This movie is not on Letterboxd."), nil
	case errors.Is(err, errWriteRefused):
		p.logger.Warn("letterboxd refused a watchlist change with a fresh session", "lid", target.lid)
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY,
			newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "Letterboxd refused this change; it will be retried.")), nil
	case errors.Is(err, letterboxd.ErrLoginRejected), errors.Is(err, letterboxd.ErrSignedOut):
		p.logger.Warn("letterboxd watchlist write lost its session", "error", err)
		return nil, faultFor(err)
	default:
		p.logger.Warn("letterboxd watchlist write failed", "error", err)
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, faultFor(err)), nil
	}
}

// errWriteRefused is a write the site refused although the session was just
// renewed, so the account is fine and only this change failed.
var errWriteRefused = errors.New("letterboxd refused the watchlist change")

// writeWatchlist sets the film's watchlist state. A refusal with the stored
// session signs in again once per request; a refusal right after a successful
// sign-in is about this film, not the account.
func (p *Provider) writeWatchlist(ctx context.Context, sess *session, target writeTarget, in bool) error {
	err := sess.client.SetInWatchlist(ctx, target.lid, target.slug, in)
	if !errors.Is(err, letterboxd.ErrSignedOut) {
		return err
	}
	if !sess.relogged {
		p.logger.Info("letterboxd write was refused; signing in again", "username", sess.creds.account.Username)
		if err := sess.relogin(ctx); err != nil {
			return err
		}
		err = sess.client.SetInWatchlist(ctx, target.lid, target.slug, in)
		if !errors.Is(err, letterboxd.ErrSignedOut) {
			return err
		}
	}
	return errWriteRefused
}

// writeTarget is the Letterboxd film a watchlist event changes.
type writeTarget struct {
	lid  string
	slug string // optional; only used as the Referer
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
			target.slug = film.Slug
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
		return writeTarget{lid: film.LID, slug: film.Slug, tv: film.TMDBType == "tv"}, nil
	}
	return writeTarget{}, letterboxd.ErrNotFound
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

func onlyWatchlist(kinds []pluginv1.WatchSyncRemoteStateKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, kind := range kinds {
		if kind != pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST {
			return false
		}
	}
	return true
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
	case errors.Is(err, letterboxd.ErrLoginRejected):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			"Letterboxd rejected the username or password.")
	case errors.Is(err, letterboxd.ErrSignedOut):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			"Could not stay signed in to Letterboxd; connect Letterboxd again.")
	case errors.Is(err, letterboxd.ErrForbidden):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED,
			"Letterboxd denied access to the watchlist.")
	case errors.Is(err, letterboxd.ErrNotFound):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
			"The Letterboxd member or page was not found.")
	case errors.Is(err, errFilmPageMissing):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"A film on the Letterboxd watchlist could not be read; the sync will be retried.")
	case errors.Is(err, letterboxd.ErrIncompleteWatchlist):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"The Letterboxd watchlist changed while it was read; the sync will be retried.")
	case errors.Is(err, letterboxd.ErrUnexpectedPage):
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"Letterboxd returned a page the plugin could not read; its layout may have changed.")
	default:
		return newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
			"Letterboxd could not be reached; the sync will be retried.")
	}
}
