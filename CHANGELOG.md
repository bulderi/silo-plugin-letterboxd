# Changelog

Notable changes to the Letterboxd watch-provider plugin. Versions follow
`version` in `manifest.json`; each release is tagged `v<version>`.

## Unreleased

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
