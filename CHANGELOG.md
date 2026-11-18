# Changelog

Notable changes to the Letterboxd watch-provider plugin. Versions follow
`version` in `manifest.json`; each release is tagged `v<version>`.

## Unreleased

- Ratings sync both ways for movies. Letterboxd's half stars map exactly to
  Silo's 1-to-10 scale. A rating made in Silo is sent once the profile has
  finished the film, because rating a film on Letterboxd marks it watched.
- Rating reads are complete snapshots checked against the count Letterboxd
  reports, like watchlist reads, and fail rather than return a partial list.
- When a rating differs on both sides, including on the first sync,
  Letterboxd's is kept: ratings Silo already had never overwrite Letterboxd's.
- `cmd/lbcheck` reads the ratings, and `-rate` rates one film, reads it back,
  and removes the rating.
- Diary entries come into Silo as plays. With the new connection setting **Log
  plays in the Letterboxd diary**, off by default, movies finished in Silo are
  logged in the diary, dated in the server's time zone, marked as a rewatch
  when the diary already has an earlier entry for the film, and with the
  rating: the film's rating on Letterboxd when the play is logged, and a
  rating given or removed in Silo within a week afterwards. Entries changed by
  the member are left alone. Plays from more than a day before the plugin
  first sees one to log are skipped, and a film already logged that day is not
  logged again.
- `cmd/lbcheck` reads the diary, and `-log` logs one film for today, rates the
  entry, reads it back, and deletes it again.
- Ratings and diary reads keep a cursor, so a sync reads only the newest pages.
  A full ratings read, which shows removed ratings, runs once a day, and a full
  diary read once a week.
- `cmd/lbcheck -rerate` changes an older rating and sets it back, to check the
  order the short ratings reads rely on.
- An unchanged watchlist is read with one page request instead of all of
  them: when the first page and the entry count match the last full read,
  that read is served. A full read still runs once a day.
- A write loads the film's page for its CSRF cookie instead of the front
  page, which Cloudflare challenges far more often.
- Development builds can load challenged ratings, diary, and film pages
  through a FlareSolverr service set at build time (`FLARESOLVERR_URL`), for
  a server whose IP address Cloudflare keeps challenging. Release builds do
  not use it.

## 0.4.0 — 2026-11-09

- New connections use the browser's `letterboxd.user` cookie instead of a
  password, because a Cloudflare challenge now blocks the sign-in page. The
  username is still required, and an email address no longer works in its place.
- A new connection is accepted only if the cookie is signed in as the given
  username.
- When Letterboxd stops accepting the cookie, Silo asks for a new one.
- The client refuses a redirect that leaves letterboxd.com or HTTPS.
- Connections made with a password keep working until they are reconnected.
- `cmd/lbcheck` takes `LETTERBOXD_COOKIE`.

## 0.3.1 — 2026-11-09

- Fixes known vulnerabilities by building with Go 1.26.10 and updating `grpc`,
  `x/net`, `x/crypto`, `x/text`, `brotli`, `klauspost/compress`, and `circl`.
- Builds against `silo-plugin-sdk` v0.32.1.
- Install from a plugin repository: `index.json` lists each platform binary
  with its SHA-256, so Silo installs the right binary and offers updates.
  Releases generate it and publish it on `main`.
- Releases run the CI checks first, use actions pinned to commit SHAs, and only
  publish from a tag that matches `manifest.json`.
- An end-to-end test calls the plugin over gRPC the way Silo does, and the
  manifest test runs the same bootstrap as the plugin.
- Added `SECURITY.md` and `CODEOWNERS`.

## 0.3.0 — 2026-10-09

First public release.

- Two-way watchlist sync with Letterboxd, movies only, in Letterboxd's order.
- Sign-in with username or email and password; sessions renew themselves.
- Complete-snapshot reads with an entry-count check, so a failed or short read
  never removes films from Silo.
- Film-to-ID cache in the plugin's instance state.
- Sync warnings for skipped TV entries, films without a TMDB or IMDb link, and
  films served from saved IDs.
- Built against `silo-plugin-sdk` v0.24.0, with the manifest presentation
  block, AGPL-3.0-only license, CI, and tag-driven releases.
