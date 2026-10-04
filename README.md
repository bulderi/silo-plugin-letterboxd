# silo-plugin-letterboxd

A private [Silo](https://siloserver.org) plugin that syncs a Letterboxd
watchlist with a Silo profile's watchlist, in both directions.

- **Plugin ID:** `bulderi.letterboxd`
- **Capability:** `watch_sync_provider.v1`, id `letterboxd`
- **Syncs:** the watchlist only, movies only. TV series in the Silo watchlist
  are never sent to Letterboxd and never removed by the sync; TV entries on
  Letterboxd are skipped.

Letterboxd has no public API for personal projects, so the plugin uses the
website the way a signed-in browser does. It is for private use.

## Connecting

1. Upload the plugin binary for the server's platform in Silo's admin plugin
   settings.
2. In the profile's watch-provider settings, connect **Letterboxd**: the
   username (or email) goes in the username field, the Letterboxd password in
   the API-key field.
3. Turn on watchlist import, export, and removal sync as wanted. Watchlist
   order sync mirrors Letterboxd's order (newest first); items Letterboxd does
   not have, such as series, sort after the films.

Silo stores the credentials encrypted. The password is kept so the plugin can
sign in again when the Letterboxd session expires.

## How it works

| Direction | What happens |
|---|---|
| Letterboxd → Silo | Every watchlist page is read signed in (private watchlists work), then each film page is read once for its TMDB and IMDb ids. Silo matches films to the library by those ids. |
| Silo → Letterboxd | The Silo item's TMDB id (or IMDb id) is resolved through `letterboxd.com/tmdb/<id>/`, then the site's own `PATCH /api/v0/me/watchlist/<film>` sets `inWatchlist`. |

Details that matter:

- **Complete snapshots only.** A read that fails anywhere (network, rate limit,
  Cloudflare challenge, a page the parser does not understand) returns a fault
  and no items. A partial list would look like removals and delete films from
  Silo. The watchlist page reports its entry count (`data-num-entries`); a read
  whose films do not add up to it fails, and a page without films counts as an
  empty watchlist only when it reports zero entries.
- **Consistent traversals.** The whole watchlist is read before any film is
  handed to Silo, then served from memory, so a film removed on Letterboxd
  while Silo pages through the result cannot shift the pages. Page tokens are
  single-use and belong to one account.
- **Film pages that disappear.** A watchlisted film whose page answers 404
  keeps its last cached ids; without any, the read fails rather than dropping
  the film, which Silo would treat as a removal.
- **Film cache.** Film → id lookups are cached in the plugin's instance state
  (64 shards, within the host's key and size limits): movies for 180 days,
  other entries for 30. The cache is only written back once it has been read,
  so a restart while the host store is unreachable cannot overwrite it. After
  the first sync, a sync costs one request per watchlist page.
- **Pacing.** One request every 1.5 s across all connected accounts. Page reads
  and film lookups each spend at most 60 s per host page, so a large watchlist
  spreads over many pages instead of hitting the host's two-minute call limit.
  The first sync takes about 1.5 s per film; a manual sync stops after Silo's
  10-minute limit, and the next one continues from the cache.
- **Blocks.** A Cloudflare challenge pauses the connection for an hour and a
  429 for its `Retry-After`. The plugin never tries to solve a challenge.
- **Sessions.** The session cookie is stored as the access token, the
  remember-me cookie as the refresh token, and the CSRF cookie with the
  account, so a write needs no extra page load. A rotated cookie or a fresh
  sign-in is returned to Silo as updated credentials. A write refused right
  after a fresh sign-in is retried later for that film only; it does not mark
  the connection as needing a new password.
- **Silo's list sync order.** Silo imports before it sends pending local
  removals, so a film removed in Silo while watchlist removal sync is off comes
  back on the next sync while it is still on Letterboxd. Keep removal sync on.

## Troubleshooting

`cmd/lbcheck` runs the plugin's client from the command line, which shows
whether a machine can sign in, read the watchlist, and write:

```sh
LETTERBOXD_USERNAME=... LETTERBOXD_PASSWORD=... go run ./cmd/lbcheck
LETTERBOXD_USERNAME=... LETTERBOXD_PASSWORD=... go run ./cmd/lbcheck -write -tmdb 11
```

`-write` adds the given movie and removes it again, and refuses a movie that is
already on the watchlist.

## Development

```sh
make test        # go test -race ./...
make build       # local binary
make build-all   # dist/ binaries for linux amd64/arm64 and darwin arm64
```

Tests run against `internal/fakesite`, an in-memory stand-in for
letterboxd.com that serves the same markup as the real pages, with invented
films and members.

```
main.go               serve the plugin; instance-state adapter
provider/             watch_sync_provider.v1: auth, snapshots, film cache, events
letterboxd/           HTTP client (Chrome TLS fingerprint) and HTML parsing
internal/fakesite/    fake letterboxd.com for tests
cmd/lbcheck/          live diagnostics
```
