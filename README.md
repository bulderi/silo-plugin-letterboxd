# Letterboxd watch-provider plugin for Silo

Syncs a Silo profile's watchlist, movie ratings, and diary with a
[Letterboxd](https://letterboxd.com) account through Silo's
`watch_sync_provider.v1` plugin contract. Movies only; series in Silo are left
alone.

- **Plugin ID:** `bulderi.letterboxd`
- **Capability:** `watch_sync_provider.v1`, id `letterboxd`

Letterboxd has no public API for this, so the plugin uses the website the way a
signed-in browser does, with a Chrome TLS fingerprint because the site sits
behind Cloudflare. A change on Letterboxd's side can break it until the plugin
is updated.

## Requirements

A Silo server whose plugin host runs watchlist sync for `watch_sync_provider.v1`
plugins. The sync warnings below show on servers that include
[Silo-Server/silo-server#1800](https://github.com/Silo-Server/silo-server/pull/1800),
in the `latest` image since October 2026; older servers sync the same way and
ignore the warnings.

## Capabilities

- Imports the Letterboxd watchlist into the Silo watchlist, in Letterboxd's
  order (newest first).
- Exports watchlist adds made in Silo to Letterboxd.
- Removes films on the other side when watchlist removal sync is on.
- Imports Letterboxd ratings into Silo and sends ratings set, changed, or
  removed in Silo to Letterboxd, in half stars (see [Ratings](#ratings)).
- Imports diary entries into Silo as plays and logs movies finished in Silo in
  the diary (see [Diary](#diary)).
- Reports skipped entries (TV titles, films without a TMDB or IMDb link) as sync
  warnings that Silo shows with the sync run.

Films marked watched on Letterboxd without a diary entry, likes, and lists are
not synced, and the other options Silo shows on the connection have no effect
for Letterboxd.

## Install

1. In Silo, open the administrator **Plugins** page, select **Catalog**, and
   click **Add repository**. Name it, for example, **Letterboxd**, and use this
   URL:

   ```text
   https://raw.githubusercontent.com/bulderi/silo-plugin-letterboxd/main/index.json
   ```

2. Find **Letterboxd Watch Provider** in the catalog and click **Install**.

Silo downloads the binary for its platform and checks its SHA-256, and new
releases appear as updates. Binaries are published for Linux amd64, Linux
arm64, and macOS arm64.

An installation uploaded by hand is not linked to the repository: Silo checks
only repository installations for updates, and the catalog shows the plugin as
already installed. To link it without reconnecting anyone, add the repository,
then install the same version from it through the admin API. Silo replaces the
installation in place, so its ID, the film cache, and every Letterboxd
connection stay:

```sh
curl -X POST "$SILO_URL/api/v2/admin/plugins/installations" \
  -H "Authorization: Bearer $SILO_ADMIN_API_KEY" -H "Content-Type: application/json" \
  -d '{"repository_id": "<id>", "plugin_id": "bulderi.letterboxd", "version": "<version>"}'
```

`GET /api/v2/admin/plugins/repositories` lists the repository ID. The API
takes it as a string, and the call needs an unscoped API key owned by an
administrator. Uninstalling and installing from the
catalog also links it, but the new installation needs every profile to connect
Letterboxd again.

## Setup

1. Sign in at letterboxd.com in a browser and copy the value of the
   `letterboxd.user` cookie. In Chrome it is under **Developer Tools →
   Application → Cookies → https://letterboxd.com**. Do not copy
   `letterboxd.user.CURRENT`.
2. In a profile's watch-provider settings, connect **Letterboxd**: your username
   goes in the username field, not your email, and the cookie value in the
   field Silo labels **API key**. The plugin has no server-wide settings.
3. Turn on **Sync watchlist removals**. Silo imports before it sends pending
   local removals, so with removal sync off, a film removed in Silo comes back
   on the next sync while it is still on Letterboxd.
4. **Mirror watchlist order** keeps Silo in Letterboxd's order. Titles that are
   not on Letterboxd, such as series, sort after the films.
5. **Import ratings** and **Send ratings** turn each direction of rating sync
   on or off. Check them on a connection made before the plugin synced
   ratings, which may have them off. The watched import option does the same
   for reading the diary.
6. **Log plays in the Letterboxd diary** is off by default. Turn it on to have
   movies you finish in Silo logged in the diary. Silo's watched export option
   must be on too; Silo turns that on for every connection, so this switch is
   what decides.

Silo stores the username and cookie encrypted with the profile's connection.
The cookie gives full access to the Letterboxd account, so treat it like a
password. The plugin keeps it and page contents out of every log line and error
message it returns. Signing out of Letterboxd in that browser can end the
cookie. When Letterboxd stops accepting it, connect again with a new one.

The first sync looks up every watchlisted, rated, and logged film once, at
about 1.5 seconds per film. A manual sync stops after Silo's 10-minute limit; the next sync continues
from the cache, and later syncs take seconds.

## How sync works

| Direction | What happens |
|---|---|
| Letterboxd → Silo | Every watchlist page is read with the member's session, so private watchlists work. Each film's page is read once for its TMDB and IMDb IDs, which Silo uses to match the library. |
| Silo → Letterboxd | The Silo item's TMDB ID (or IMDb ID) is resolved through `letterboxd.com/tmdb/<id>/`, then the site's own `PATCH /api/v0/me/watchlist/<film>` sets `inWatchlist`. |
| Letterboxd ratings → Silo | `/<member>/films/rated/.5-5/by/rated-date/` is read, most recently rated first; each film tile carries the member's rating. |
| Silo ratings → Letterboxd | The film is resolved the same way, then the site's own `PATCH /api/v0/me/rate/<film>` sets `rating` in half stars, or `null` to remove it. |
| Letterboxd diary → Silo | `/<member>/diary/films/` is read, newest entry first; each row is one entry with its date. |
| Silo plays → Letterboxd diary | The film is resolved the same way, the day's diary page is checked, then the site's own `POST /api/v0/production-log-entries` logs the film for that date. |

- **Complete snapshots only.** A read that fails anywhere (network, rate limit,
  Cloudflare challenge, a page the parser does not understand) returns a fault
  and no items, because Silo treats a film missing from a complete snapshot as
  removed. The watchlist page reports its entry count (`data-num-entries`); a
  read whose films do not add up to it fails, and a page without films counts as
  an empty watchlist only when it reports zero entries.
- **Consistent traversals.** The whole watchlist is read before any film is
  handed to Silo and is then served from memory, so a film removed on Letterboxd
  while Silo pages through the result cannot shift the pages. Page tokens are
  single-use and belong to one account.
- **Film pages that disappear.** A watchlisted film whose page answers 404 keeps
  its last cached IDs, with a sync warning; without any, the read fails rather
  than dropping the film.
- **Film cache.** Film-to-ID lookups are cached in the plugin's instance state
  (64 shards, within the host's key and size limits): movies for 180 days, other
  entries for 30. The cache is written back only once it has been read, so a
  restart while the host store is unreachable cannot overwrite it.
- **Writes** are convergent desired-state updates, so a retried event is
  harmless.
- **Sessions.** A new connection stores the `letterboxd.user` cookie and never
  opens the sign-in page, which sits behind a Cloudflare challenge. The cookie
  is stored as both the access and refresh token until the site sets a session
  cookie. Each write call loads the front page once for a fresh CSRF cookie,
  because the plugin cannot sign in again to replace a stale one. A rotated
  cookie is returned to Silo as updated credentials.
- **Cookie checks.** A new connection is accepted only if the Letterboxd
  settings page shows that the cookie is signed in as the given username, which
  rejects a dead cookie, another member's cookie, and a pasted password. When a
  write is refused, a write for a film that cannot exist shows whether the
  cookie still works without changing anything: a working cookie gets the film
  retried, a dead one reports the connection as needing a new cookie. A public
  watchlist reads fine with a dead cookie, so a cookie that dies later shows at
  the next write.
- **Connections made before 0.4.0** used a password and keep working. They sign
  in again when their session expires, and a Cloudflare challenge on that
  sign-in pauses the sync.

### Ratings

Letterboxd rates in half stars from ½ to 5, and Silo and the plugin contract
use whole numbers from 1 to 10, so a rating maps exactly: 3½ stars is 7. Silo
servers that store whole stars round on their side (see
[Silo-Server/silo-server#4398](https://github.com/Silo-Server/silo-server/pull/4398)).

- **Reads** follow the watchlist rules: a complete snapshot of every rated
  film, checked against the count the page heading reports ("watched 1,204
  films rated between ½ and ★★★★★"). A tile without a rating, a short read, or
  a page without films that does not say the member has rated none fails the
  read, because Silo treats a film missing from a complete snapshot as unrated.
  Letterboxd's pages do not say when a film was rated, so when both sides
  change the same rating between syncs, Silo's wins.
- **Writes** set the desired rating, so a retry is harmless. Removing a rating
  that is not there, or one for a film Letterboxd does not have or lists as TV,
  answers "no change" rather than an error, as the contract requires.
- **Rating marks a film watched on Letterboxd.** The manifest lists movies in
  `rating_export_requires_watched`, so Silo sends a new rating only once the
  profile has a completed play of the film. Removing the rating leaves the film
  marked watched on Letterboxd.

### Diary

- **Reads** return one row per film: how many diary entries it has, and the
  newest entry's date. Silo imports the newest play of a title. A diary entry
  has a date but no time, so the plugin reads it as the start of that day in
  the Silo server's time zone. The diary is history, not a snapshot: an entry
  missing from a read is not a removal, and Silo never removes plays on
  import. A film whose page is gone is skipped with a warning.
- **Writes** happen only when the connection has **Log plays in the
  Letterboxd diary** on (`log_plays_in_diary`), which is off by default: Silo
  turns watched export on for every connection, and an entry is public and
  cannot be removed by the plugin. With the switch off, a play is answered "no
  change" and never logged later. With it on, the plugin logs a completed Silo
  play as a diary entry for the play's date in the server's time zone (`TZ`),
  with the film's rating on Letterboxd if the member set one (a rating the
  site derived is left out), as the site's Log form fills it in, and no review, tags, or like. A diary entry is not desired state,
  since each call adds one, so the plugin first reads that day's diary page
  and answers "no change" when the film is already there. A retried play
  therefore never adds a second entry, and neither does a second play of the
  same film that day.
- **Rewatches.** The plugin asks the site's own
  `/s/check-viewingable-relation` for the member's earliest diary entry of the
  film, as the Log form does, and marks the entry as a rewatch when that entry
  is on or before the play's date. A film only marked watched, with no diary
  entry, counts as a first watch, as it does on the site. If the answer cannot
  be read or is refused, the play is logged without the flag rather than held
  back, and the write itself shows whether the cookie still works; a rate
  limit or Cloudflare check retries the play.
- **Ratings on entries.** A rating usually comes after the play, so the
  entry is logged before it exists. The plugin records the entries it logs,
  per account in its instance state, and when Silo sends a rating, or removes
  one, within a week of logging, it puts that rating on the film's newest
  entry it logged; an earlier viewing's entry keeps the rating it had then.
  The diary then reads "watched on this date, rated this". It reads the entry
  first and changes only the rating, keeping the date and rewatch flag. It
  leaves an entry alone when someone changed it after the plugin's last write
  (its `whenUpdated` differs), when it has a review, tags, or a like, and when
  diary logging is off. Ratings after a week change the film's rating only, as
  rating a film on the site does. The entry is updated before the film, so a
  failed entry update retries the whole rating.
- **Only new plays.** Silo queues every completed play in the profile's history
  when watched export is turned on. The plugin records, per Letterboxd account
  in its instance state, when it first saw a play to log, and answers "no
  change" for plays from more than a day before then, so the export does not
  add your whole history to the diary and your followers' activity. The day of
  grace lets the play that started the export, and a film finished just before
  it was turned on, still reach the diary. Connecting again with a new cookie
  keeps that moment.
- **No echo.** Silo does not send a play back to the connection it was imported
  from. An entry the plugin logged reads back at the start of its day, which is
  never later than the Silo play, so Silo does not record it as a second play.
- Private diary entries may be missing from the day's page if Letterboxd shows
  the plugin only public ones; the plugin cannot tell.

### Short reads

Ratings and diary reads hand Silo a cursor, which Silo keeps with the
connection and sends with the next read, so a sync reads only the newest pages:

- **Ratings** come most recently rated first, and changing a rating moves the
  film to the top. The cursor remembers the 20 most recently rated films and
  their ratings, and the next read stops after the first page that holds one
  of them unchanged. Such a read is not a snapshot, so it cannot show a rating
  removed on Letterboxd; once a day the read is a full one, a complete
  snapshot, which does. A short read skips a film whose page is gone, with a
  warning; a full read fails instead, as the watchlist does.
- **Diary** reads stop after the first page that reaches two weeks before the
  newest entry the last read saw. An entry logged later for an older date
  comes in with the full read once a week.
- Silo saves the cursor only after it has taken every item of a read, and a
  cursor the plugin cannot read means a full read. Silo also drops the cursor
  when the connection moves to another account, and when rating import is
  turned on.

### Rate limits

The plugin sends at most one request every 1.5 seconds across all connected
accounts. Page reads and film lookups each spend at most 60 seconds per host
page, so a large watchlist, ratings list, or diary spreads over many pages instead of hitting the host's
two-minute call limit. A Cloudflare challenge pauses the connection for an hour,
and a 429 for its `Retry-After`. The plugin never tries to solve a challenge.
A batch of changes stops starting new ones 30 seconds before Silo's deadline
for the call, since a diary play can take several requests; the rest are
retried.

## Not yet supported

- An empty Letterboxd watchlist's page without an entry count. Letterboxd
  reports the count today; without it, an empty page reads as a failed read.

## Troubleshooting

`cmd/lbcheck` runs the plugin's client from the command line and shows whether a
machine can read the watchlist, ratings, and diary and write with a cookie:

```sh
read -rs LETTERBOXD_COOKIE; export LETTERBOXD_COOKIE
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -write -tmdb 11
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rate -tmdb 11
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rerate -tmdb 11
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -log -tmdb 11
```

`read -rs` asks for the cookie without showing it, which keeps it out of the
shell history. The read also asks the rewatch check about the film of the
oldest diary entry, which changes nothing.

`-write` adds the given movie and removes it again, and refuses a movie that is
already on the watchlist. `-rate` rates the given movie 3½ and then 4 stars,
reads each rating back, and removes it; it refuses a movie that is already
rated. Letterboxd keeps a rated movie marked watched after the rating is
removed, so pick one you have seen. `-rerate` takes a movie you rated earlier,
changes its rating by half a star, checks that it becomes the most recently
rated, which the short ratings reads rely on, and sets your rating back; the
movie then stays first in that order. `-log` logs the given movie in the diary
for today the way the plugin does, rates the entry 3½ stars the way a later
Silo rating would, reads it back, and then deletes the entry and the film
rating it gave; it refuses a movie that is rated or already logged today, and
the movie stays marked watched. The read alone also checks that the rewatch
check reports the same rating as the ratings list. Without `-write`, `-rate`,
`-rerate`, or `-log`,
`cmd/lbcheck` does not prove that the cookie works. `-debug` shows how the site's pages
describe the session, with every value but the username redacted. With
`LETTERBOXD_PASSWORD` instead of the cookie, `cmd/lbcheck` tries the password
sign-in.

## Development

The plugin builds against `silo-plugin-sdk` v0.32.1.

```sh
make test        # go test -race ./...
make build       # local binary
./plugin manifest
make build-all   # dist/ binaries for the platforms in manifest.json
```

The build takes its version from `manifest.json`, so Silo's plugin admin shows
which build is installed. The plugin starts through the SDK's manifest
bootstrap (`runtime.ServeManifest`), which validates the manifest, stamps the
version and the binary's checksum, and answers `plugin manifest`.

Tests run against `internal/fakesite`, an in-memory stand-in for letterboxd.com
that serves the same markup as the real pages, with invented films and members.
`grpc_test.go` serves the plugin through the SDK's plugin set and calls it over
gRPC, the way the host does.

```
main.go               serve the plugin; instance-state adapter
provider/             watch_sync_provider.v1: auth, snapshots, film cache, events, warnings
letterboxd/           HTTP client (Chrome TLS fingerprint), HTML parsing, list reader
internal/fakesite/    fake letterboxd.com for tests
cmd/lbcheck/          live diagnostics
cmd/genindex/         writes index.json for a release (internal/catalogindex)
```

To release, bump `version` in `manifest.json`, add the changes to
[CHANGELOG.md](CHANGELOG.md), and push a matching tag (`v0.3.1`); there is no
automatic version bump. The release workflow runs the CI checks, builds every
platform, and publishes the binaries with `checksums.txt` and `index.json`,
written by `cmd/genindex`. It then commits that `index.json` to `main`, which
is the file the install URL serves. [CONTRIBUTING.md](CONTRIBUTING.md) lists
the checks to run first.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Changes to
authentication, snapshot completeness, idempotency, or the watch-sync contract
should start as an issue.

## License

AGPL-3.0-only.
