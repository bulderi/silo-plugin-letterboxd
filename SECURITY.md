# Security policy

## Supported versions

Only the latest release receives fixes. Update through the plugin repository
in Silo, or upload the latest release binary.

## Reporting a vulnerability

Report security problems privately through
[GitHub's private vulnerability reporting](https://github.com/bulderi/silo-plugin-letterboxd/security/advisories/new),
not in a public issue. Include the plugin version, the Silo version, and the
steps to reproduce. Never include real Letterboxd passwords, session cookies, or
CSRF tokens; replace them with placeholders.

## What the plugin handles

- **Credentials.** Silo stores each connection's Letterboxd username and
  cookies encrypted and passes them to the plugin only for the duration of a
  call. The `letterboxd.user` cookie gives full access to the account. The
  plugin never writes the cookies or page contents to logs or error messages.
- **Network access.** The plugin talks only to `letterboxd.com`. A
  development build can be given a FlareSolverr service at build time; it
  then also sends that service the addresses of challenged public pages,
  never cookies.
- **Stored data.** The plugin's own instance state holds a cache of public film
  identifiers (Letterboxd film ID, TMDB and IMDb IDs, title, year) and, per
  Letterboxd account, the last full watchlist read and the diary entries the
  plugin logged. It holds no cookies. Silo encrypts instance state at rest.

A problem in any of these, such as a credential reaching a log line or a
request going to another host, is a security issue.
