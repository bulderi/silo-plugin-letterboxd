# Letterboxd watch-provider plugin for Silo

> [!WARNING]
> This is an unofficial plugin, not made or endorsed by Letterboxd or Silo. It
> changes your Letterboxd watchlist, ratings, and diary. Use it at your own
> risk: it comes without warranty, and the author is not responsible for lost
> or changed data on Letterboxd or in Silo, or for anything Letterboxd does to
> your account. Consider
> [exporting your Letterboxd data](https://letterboxd.com/settings/data/)
> before you connect.

Keeps your Silo watchlist, movie ratings, and diary in sync with your
[Letterboxd](https://letterboxd.com) account, in both directions. Movies only;
series in Silo are left alone.

## What syncs

| | Letterboxd → Silo | Silo → Letterboxd |
|---|---|---|
| **Watchlist** | Yes | Yes |
| **Ratings** | Yes | Yes\* |
| **Diary** | Yes, as plays | Optional, off by default |
| **Likes, lists, reviews** | No | No |

\* Rating a film on Letterboxd also marks it as watched. So a new rating for a
film you have not finished in Silo waits, and is sent once you finish it. You
can rate first or watch first. Changing a rating Letterboxd already has is
sent right away.

## Install

1. In Silo, open the administrator **Plugins** page, select **Catalog**, and
   click **Add repository** with this URL:

   ```text
   https://raw.githubusercontent.com/bulderi/silo-plugin-letterboxd/main/index.json
   ```

2. Find **Letterboxd Watch Provider** in the catalog and click **Install**.

New releases show up as updates in Silo.

## Setup

1. Sign in at letterboxd.com in a browser and copy the value of the
   `letterboxd.user` cookie (in Chrome: **Developer Tools → Application →
   Cookies → https://letterboxd.com**). Not `letterboxd.user.CURRENT`.
2. In a profile's watch-provider settings, connect **Letterboxd** with your
   username (not your email) and the cookie in the field Silo labels **API
   key**.
3. Turn on **Sync watchlist removals**. Without it, a film you remove in Silo
   comes back on the next sync.
4. Optional: turn on **Mirror watchlist order** to keep Letterboxd's order, and
   **Log plays in the Letterboxd diary** to log movies you finish in Silo.

The cookie gives full access to your Letterboxd account, so treat it like a
password. Silo stores it encrypted and the plugin never logs it. If you sign out
of Letterboxd in that browser, the cookie may stop working; Silo then asks you
to connect again.

## Good to know

- **The first sync is slow.** It looks up every film once, about 1.5 seconds
  each. Later syncs take seconds.
- **A failed sync changes nothing.** If Letterboxd cannot be read completely,
  Silo keeps what it has.
- **Letterboxd wins rating conflicts.** If a rating changed on both sides, the
  Letterboxd rating is kept. Half stars convert exactly: 3½ stars is 7 in Silo.
- **Diary entries are public, and the plugin cannot delete them.** It logs only
  new plays, never your old history, and at most once per film per day.
  Entries are dated in the Silo server's time zone, its `TZ` setting, so set
  `TZ` (for example `TZ=Europe/Stockholm`) if the server runs in UTC. A
  rating you give in Silo within a week is added to the entry. Entries you
  edited yourself are left alone.
- **Letterboxd sometimes blocks servers.** The site sits behind Cloudflare. When
  it blocks a sync, the plugin waits an hour and tries again. If that happens
  often, see [FlareSolverr](#flaresolverr).
- **Skipped items show as sync warnings**, for example TV titles or films
  Letterboxd cannot link to TMDB or IMDb.
- Letterboxd has no public API, so the plugin uses the website like a browser
  does. A change on Letterboxd's side can break it until the plugin is updated.
- Connections made with a password before 0.4.0 must be connected again with a
  cookie.

## FlareSolverr

Cloudflare blocks servers in data centers far more often than home servers.
If it keeps pausing your sync, run a
[FlareSolverr](https://github.com/FlareSolverr/FlareSolverr) container that
Silo can reach, and enter its address, such as `http://flaresolverr:8191`, in
the plugin's settings. Blocked rating, diary, and film pages then load through
its browser instead.

FlareSolverr never gets anyone's cookie, so it only loads public pages. The
watchlist and the check before a diary entry can hold private entries, so they
never go through it. Leave the setting empty to talk only to letterboxd.com.

---

## For developers

- **Plugin ID:** `bulderi.letterboxd`
- **Capability:** `watch_sync_provider.v1`, id `letterboxd`

### How sync stays safe

- A read that fails anywhere returns no items, because Silo treats a film
  missing from a complete read as removed. Watchlist and rating reads are
  checked against the count Letterboxd reports.
- Writes set the desired state, so a retry does nothing new. A diary play is
  logged only after the day's diary page shows the film is not there yet.
- Plays from more than a day before the first one the plugin was asked to log
  are skipped. An entry's rating is changed only if nobody else changed the
  entry and it has no review, tags, or like.
- At most one request every 1.5 seconds across all accounts. A Cloudflare
  challenge pauses the connection for an hour, and a 429 for its `Retry-After`.
- Most syncs read only the newest pages. A full read runs daily for the
  watchlist and ratings, and weekly for the diary.
- If Letterboxd shows the plugin only public diary entries, a private entry for
  the same day is missed and the film can be logged twice.

### Build and test

```sh
make test        # go test -race ./...
make build       # local binary
make build-all   # dist/ binaries for the platforms in manifest.json
```

Tests run against `internal/fakesite`, a fake letterboxd.com with invented films
and members. `grpc_test.go` calls the plugin over gRPC the way Silo does.

```
main.go               serve the plugin; instance-state adapter
provider/             watch_sync_provider.v1: auth, snapshots, film cache, events, warnings
letterboxd/           HTTP client (Chrome TLS fingerprint), HTML parsing, list reader
internal/fakesite/    fake letterboxd.com for tests
cmd/lbcheck/          live diagnostics
cmd/genindex/         writes index.json for a release (internal/catalogindex)
```

### Live check

`cmd/lbcheck` runs the plugin's client against the real site:

```sh
read -rs LETTERBOXD_COOKIE; export LETTERBOXD_COOKIE
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck               # read everything
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -write -tmdb 11
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rate -tmdb 11
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -rerate -tmdb 11
LETTERBOXD_USERNAME=... go run ./cmd/lbcheck -log -tmdb 11
```

A read alone does not prove the cookie works; a write flag does, and undoes
its change afterwards. `-write` adds and removes a watchlist film, `-rate`
rates and unrates one, `-rerate` changes one of your ratings and sets it back,
and `-log` logs, rates, and deletes a diary entry. A rated movie stays marked
watched, so pick one you have seen. `-debug` shows how the site describes the
session, with values redacted. `-flaresolverr <url>` loads blocked pages
through FlareSolverr, as the plugin's setting does.

### Manual installs

An installation uploaded by hand gets no updates. To link it to the repository
without reconnecting anyone, add the repository and install the same version
from it through the admin API, with an administrator's unscoped API key. The
repository ID comes from `GET /api/v2/admin/plugins/repositories`.

```sh
curl -X POST "$SILO_URL/api/v2/admin/plugins/installations" \
  -H "Authorization: Bearer $SILO_ADMIN_API_KEY" -H "Content-Type: application/json" \
  -d '{"repository_id": "<id>", "plugin_id": "bulderi.letterboxd", "version": "<version>"}'
```

Sync warnings need a Silo server with
[Silo-Server/silo-server#1800](https://github.com/Silo-Server/silo-server/pull/1800).
Binaries are published for Linux amd64, Linux arm64, and macOS arm64.

### Release

Bump `version` in `manifest.json`, add the changes to
[CHANGELOG.md](CHANGELOG.md), and push a matching tag (`v0.5.0`). The release
workflow builds every platform, publishes the binaries with `checksums.txt`,
and commits the new `index.json` to `main`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Changes to
authentication, snapshot completeness, idempotency, or the watch-sync contract
should start as an issue.

## License

AGPL-3.0-only.
