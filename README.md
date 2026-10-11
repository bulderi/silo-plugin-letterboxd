# Letterboxd watch-provider plugin for Silo

Syncs a Silo profile's watchlist with a [Letterboxd](https://letterboxd.com)
watchlist through Silo's `watch_sync_provider.v1` plugin contract. Movies only;
series in the Silo watchlist are left alone.

- **Plugin ID:** `bulderi.letterboxd`
- **Capability:** `watch_sync_provider.v1`, id `letterboxd`

Letterboxd has no public API for this, so the plugin uses the website the way a
signed-in browser does, with a Chrome TLS fingerprint because sign-in sits
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
- Reports skipped entries (TV titles, films without a TMDB or IMDb link) as sync
  warnings that Silo shows with the sync run.

Letterboxd watched history, ratings, likes, and lists are not synced, and the
other options Silo shows on the connection have no effect for Letterboxd.

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
  -d '{"repository_id": <id>, "plugin_id": "bulderi.letterboxd", "version": "<version>"}'
```

`GET /api/v2/admin/plugins/repositories` lists the repository ID. The call
needs an unscoped API key owned by an administrator. Uninstalling and installing from the
catalog also links it, but the new installation needs every profile to connect
Letterboxd again.

## Setup

1. In a profile's watch-provider settings, connect **Letterboxd**: the username
   or email goes in the username field, the Letterboxd password in the field
   Silo labels **API key**. The plugin has no server-wide settings.
2. Turn on **Sync watchlist removals**. Silo imports before it sends pending
   local removals, so with removal sync off, a film removed in Silo comes back
   on the next sync while it is still on Letterboxd.
3. **Mirror watchlist order** keeps Silo in Letterboxd's order. Titles that are
   not on Letterboxd, such as series, sort after the films.

Silo stores the username and password encrypted with the profile's connection.
The plugin keeps the password only to sign in again when the Letterboxd session
expires, and keeps it, the session cookies, and page contents out of every log
line and error message it returns.

The first sync looks up every watchlisted film once, at about 1.5 seconds per
film. A manual sync stops after Silo's 10-minute limit; the next sync continues
from the cache, and later syncs take seconds.

## How sync works

| Direction | What happens |
|---|---|
| Letterboxd → Silo | Every watchlist page is read with the member's session, so private watchlists work. Each film's page is read once for its TMDB and IMDb IDs, which Silo uses to match the library. |
| Silo → Letterboxd | The Silo item's TMDB ID (or IMDb ID) is resolved through `letterboxd.com/tmdb/<id>/`, then the site's own `PATCH /api/v0/me/watchlist/<film>` sets `inWatchlist`. |

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
  harmless. A write refused right after a fresh sign-in is retried later for
  that film only; it does not mark the connection as needing a new password.
- **Sessions.** The session cookie is stored as the access token, the
  remember-me cookie as the refresh token, and the CSRF cookie with the account,
  so a write needs no extra page load. A rotated cookie or a fresh sign-in is
  returned to Silo as updated credentials.

### Rate limits

The plugin sends at most one request every 1.5 seconds across all connected
accounts. Page reads and film lookups each spend at most 60 seconds per host
page, so a large watchlist spreads over many pages instead of hitting the host's
two-minute call limit. A Cloudflare challenge pauses the connection for an hour,
and a 429 for its `Retry-After`. The plugin never tries to solve a challenge.

## Not yet supported

- An empty Letterboxd watchlist's page without an entry count. Letterboxd
  reports the count today; without it, an empty page reads as a failed read.

## Troubleshooting

`cmd/lbcheck` runs the plugin's client from the command line and shows whether a
machine can sign in, read the watchlist, and write:

```sh
LETTERBOXD_USERNAME=... LETTERBOXD_PASSWORD=... go run ./cmd/lbcheck
LETTERBOXD_USERNAME=... LETTERBOXD_PASSWORD=... go run ./cmd/lbcheck -write -tmdb 11
```

`-write` adds the given movie and removes it again, and refuses a movie that is
already on the watchlist. `-debug` shows how the site's pages describe the
session after sign-in, with every value but the username redacted.

## Development

The plugin builds against `silo-plugin-sdk` v0.24.0.

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
letterboxd/           HTTP client (Chrome TLS fingerprint), HTML parsing, watchlist reader
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
