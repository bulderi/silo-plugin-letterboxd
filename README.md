# Letterboxd watch-provider plugin for Silo

Syncs a Silo profile's watchlist with a [Letterboxd](https://letterboxd.com)
watchlist through Silo's `watch_sync_provider.v1` plugin contract. Movies only;
series in the Silo watchlist are left alone.

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

Silo stores the username and cookie encrypted with the profile's connection.
The cookie gives full access to the Letterboxd account, so treat it like a
password. The plugin keeps it and page contents out of every log line and error
message it returns. Signing out of Letterboxd in that browser can end the
cookie. When Letterboxd stops accepting it, connect again with a new one.

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
machine can read the watchlist and write with a cookie:

```sh
read -rs LETTERBOXD_COOKIE; export LETTERBOXD_COOKIE
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -write -tmdb 11
```

`read -rs` asks for the cookie without showing it, which keeps it out of the
shell history.

`-write` adds the given movie and removes it again, and refuses a movie that is
already on the watchlist. Without it, `cmd/lbcheck` does not prove that the
cookie works. `-debug` shows how the site's pages
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
