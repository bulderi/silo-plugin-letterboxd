package provider

import (
	"context"
	"errors"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

// diaryGrace is how long before the recorded start a play still counts as
// new. The start is recorded when the first play reaches the plugin, which is
// after that play finished, so without it the play that started the export
// would never be logged; a day also covers a film finished just before the
// export was turned on.
const diaryGrace = 24 * time.Hour

// aggregateDiary turns diary entries into one entry per film, keeping the
// newest date, and counts each film's entries. Silo imports the newest play
// of a title, so one row per film carries everything it uses.
func aggregateDiary(entries []letterboxd.Poster) ([]letterboxd.Poster, map[string]int) {
	plays := map[string]int{}
	index := map[string]int{}
	var films []letterboxd.Poster
	for _, entry := range entries {
		plays[entry.LID]++
		if i, ok := index[entry.LID]; ok {
			if entry.WatchedOn > films[i].WatchedOn {
				films[i] = entry
			}
			continue
		}
		index[entry.LID] = len(films)
		films = append(films, entry)
	}
	return films, plays
}

// diaryDay is the start of a diary date in the plugin's time zone. A diary
// entry has no time, and the start of the day is never later than a Silo play
// on that date, so an entry the plugin logged for a Silo play reads back as no
// newer than that play and does not become a second one.
func (p *Provider) diaryDay(watchedOn string) (time.Time, bool) {
	day, err := time.ParseInLocation("2006-01-02", watchedOn, p.location)
	return day, err == nil
}

// applyWatched logs a completed Silo play as a diary entry dated in the
// plugin's time zone, marked as a rewatch the way the site's own Log form
// would mark it, when the connection has SettingLogPlays on; otherwise
// it answers NO_CHANGE and records nothing, so the start is set only once
// logging is on. Plays from more than diaryGrace before the account's
// diary export started are skipped, and so is a play whose film already has an entry that day, so
// a retried event never adds a second entry.
func (p *Provider) applyWatched(ctx context.Context, sess *session, event *pluginv1.WatchSyncEvent) (*pluginv1.WatchSyncApplyResult, *pluginv1.WatchSyncFault) {
	if event.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "Letterboxd sync handles movies only."), nil
	}
	if event.GetOccurredAt() == nil {
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST, "The play has no time to date the diary entry with."), nil
	}
	if !sess.logPlays {
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE, nil), nil
	}
	watchedAt := event.GetOccurredAt().AsTime()
	username := sess.creds.account.Username
	start, err := p.accounts.diaryStart(ctx, username)
	if err != nil {
		p.logger.Warn("letterboxd diary start unavailable", "error", err)
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY,
			newFault(pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, "The plugin could not read its saved state; the play will be retried.")), nil
	}
	if watchedAt.Before(start.Add(-diaryGrace)) {
		return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE, nil), nil
	}

	target, err := p.targetForEvent(ctx, sess, event)
	if err == nil && target.tv {
		return rejected(event, pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT, "Letterboxd lists this title as TV, not a movie."), nil
	}
	day := watchedAt.In(p.location)
	if err == nil {
		var logged bool
		logged, err = sess.client.DiaryHasFilm(ctx, username, target.lid, day)
		if errors.Is(err, letterboxd.ErrNotFound) {
			// The day's page is missing, not the film: retry rather than
			// reject the play.
			err = errDiaryRead
		}
		if err == nil && logged {
			return applyResult(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE, nil), nil
		}
	}
	var relation letterboxd.Relation
	if err == nil {
		relation, err = p.filmRelation(ctx, sess, target)
	}
	if err == nil {
		date, _ := time.Parse("2006-01-02", day.Format("2006-01-02"))
		rewatch := !relation.Earliest.IsZero() && !relation.Earliest.After(date)
		var entry letterboxd.LogEntry
		err = p.writeFilm(ctx, sess, func() error {
			var logErr error
			entry, logErr = sess.client.LogFilm(ctx, target.lid, target.slug, day, rewatch, relation.Rating)
			return logErr
		})
		if err == nil && entry.ID == "" {
			p.logger.Warn("letterboxd took the diary entry but its answer could not be read; a later Silo rating will not reach it", "lid", target.lid)
		} else if err == nil {
			if recordErr := p.accounts.addEntry(ctx, username, loggedEntry{LID: target.lid, ID: entry.ID, Logged: p.now().Unix(), Updated: entry.Updated}); recordErr != nil {
				p.logger.Warn("letterboxd diary entry logged but not recorded; a later Silo rating will not reach it", "error", recordErr)
			}
		}
	}
	return p.writeResult(event, target, err, false)
}

// filmRelation asks what the site's Log form asks before it shows: the
// member's earliest entry for the film, which decides "rewatch", and the
// member's rating, which the new entry takes. A film whose UID the cache
// lacks, from a lookup by an older plugin version, has its page read once for
// it. An answer the plugin cannot read, or a refusal, logs the play without
// either rather than holding it back; the write that follows shows whether
// the session is still alive. A rate limit or challenge is returned so the
// play is retried.
func (p *Provider) filmRelation(ctx context.Context, sess *session, target writeTarget) (letterboxd.Relation, error) {
	uid := target.uid
	if uid == "" && target.slug != "" {
		page, err := sess.client.Film(ctx, target.slug)
		switch {
		case err == nil:
			uid = page.UID
			film := cachedFromPage(page, p.now())
			if film.LID == target.lid {
				p.cache.put(film)
			}
		case errors.Is(err, letterboxd.ErrNotFound), errors.Is(err, letterboxd.ErrUnexpectedPage):
		default:
			return letterboxd.Relation{}, err
		}
	}
	if uid == "" {
		p.logger.Warn("letterboxd diary entry logged without its rewatch flag and rating: no film uid", "lid", target.lid)
		return letterboxd.Relation{}, nil
	}
	relation, err := sess.client.FilmRelation(ctx, uid)
	switch {
	case err == nil:
		return relation, nil
	case errors.Is(err, letterboxd.ErrNotFound), errors.Is(err, letterboxd.ErrUnexpectedPage), errors.Is(err, letterboxd.ErrForbidden):
		p.logger.Warn("letterboxd diary entry logged without its rewatch flag and rating", "lid", target.lid, "error", err)
		return letterboxd.Relation{}, nil
	default:
		return letterboxd.Relation{}, err
	}
}

// rateLoggedEntries gives the rating (1 to 10, or 0 to clear it) to the
// newest entry for the film that the plugin itself logged in the last
// diaryEntryWindow, so the entry shows the rating the member gave after
// watching. Only the newest: an earlier viewing's entry keeps the rating it
// had then. An entry changed since the plugin last wrote it, on the site or
// elsewhere, or one with a review, tags, or a like, is the member's and is
// left alone, as is every entry when diary logging is off. A rate limit or
// failed request is returned, so the rating event is retried.
func (p *Provider) rateLoggedEntries(ctx context.Context, sess *session, lid string, rating int) error {
	if !sess.logPlays {
		return nil
	}
	username := sess.creds.account.Username
	record, found, err := p.accounts.newestEntry(ctx, username, lid)
	if err != nil {
		// The rating itself still goes through; only the entry misses it.
		p.logger.Warn("letterboxd logged diary entries unavailable; the rating is not put on them", "error", err)
		return nil
	}
	if !found {
		return nil
	}
	entry, err := sess.client.GetLogEntry(ctx, record.ID)
	switch {
	case errors.Is(err, letterboxd.ErrNotFound):
		p.accounts.dropEntry(ctx, username, record.ID)
		return nil
	case errors.Is(err, letterboxd.ErrForbidden):
		// A refused read is a refused write, so the cookie check decides.
		return letterboxd.ErrSignedOut
	case err != nil:
		return err
	}
	if entry.Updated != record.Updated || !entry.Plain() || entry.FilmLID != lid {
		p.logger.Info("letterboxd diary entry changed since the plugin logged it; the rating is not put on it",
			"entry", record.ID, "lid", lid)
		return nil
	}
	if entry.Rating == rating {
		return nil
	}
	updated, err := sess.client.SetLogEntryRating(ctx, entry, rating)
	if errors.Is(err, letterboxd.ErrNotFound) {
		// Deleted after the read: the film's rating is still set.
		p.accounts.dropEntry(ctx, username, record.ID)
		return nil
	}
	if err != nil {
		return err
	}
	record.Updated = updated.Updated
	if err := p.accounts.replaceEntry(ctx, username, record); err != nil {
		p.logger.Warn("letterboxd diary entry rated but not recorded", "error", err)
	}
	return nil
}

// errDiaryRead is a day's diary page that answered 404.
var errDiaryRead = errors.New("letterboxd diary day page was not found")

// diaryEntryWindow is how long after logging an entry a Silo rating is still
// put on it. Later ratings change the film's rating only, as rating a film on
// the site does.
const diaryEntryWindow = 7 * 24 * time.Hour
