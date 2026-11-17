package provider

import (
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/bulderi/silo-plugin-letterboxd/letterboxd"
)

const (
	// ConnectionUsernameKey is the flattened connection-config field the host
	// passes to ExchangeAPIKey: config key "account", field "username".
	ConnectionUsernameKey = "account.username"

	// SettingLogPlays is the connection setting that turns diary logging on.
	// Silo turns watched export on for every connection by default, and a
	// diary entry is public and cannot be removed by the plugin, so the
	// plugin logs plays only when the profile switches this on.
	SettingLogPlays = "log_plays_in_diary"

	attrUsername = "username"
	attrLogin    = "login"
	attrPassword = "password"
	attrCSRF     = "csrf"
	tokenType    = "letterboxd-session"
)

// account is what the plugin needs to act for one Letterboxd member. The host
// stores it encrypted in the connection's credential bundle.
type account struct {
	// Username is the member's username, which is what watchlist URLs use.
	Username string
	// Login and Password exist only on connections made before the plugin
	// switched to cookies. A cookie connection has neither and cannot sign in
	// again by itself.
	Login    string
	Password string
}

// canSignIn reports whether the connection can sign in again by itself, which
// only a connection from before the switch to cookies can.
func (a account) canSignIn() bool { return a.Password != "" }

// credentials is the stored form of a connection.
type credentials struct {
	account account
	session letterboxd.Session
}

// credentialsFromContext reads the stored credential bundle. ok is false when
// the bundle is missing the account, which means the connection predates the
// plugin or was damaged; the user has to reconnect.
func credentialsFromContext(ctx *pluginv1.WatchSyncAuthenticatedContext) (credentials, bool) {
	stored := ctx.GetCredentials()
	attrs := stored.GetSecretAttributes()
	acc := account{
		Username: strings.TrimSpace(attrs[attrUsername]),
		Login:    strings.TrimSpace(attrs[attrLogin]),
		Password: attrs[attrPassword],
	}
	if acc.Login == "" {
		acc.Login = acc.Username
	}
	current, remember := stored.GetAccessToken(), stored.GetRefreshToken()
	if current == remember {
		// proto stored the remember-me cookie twice; there is no session cookie.
		current = ""
	}
	if acc.Username == "" || (!acc.canSignIn() && current == "" && remember == "") {
		return credentials{}, false
	}
	return credentials{
		account: acc,
		session: letterboxd.Session{
			Current:  current,
			Remember: remember,
			CSRF:     attrs[attrCSRF],
		},
	}, true
}

// proto builds the bundle the host persists. The access token is the session
// cookie and the refresh token the remember-me cookie. Silo refuses a bundle
// without an access token and the site sets no session cookie for a request
// carrying only the remember-me cookie, so that cookie then fills both fields.
func (c credentials) proto() *pluginv1.WatchSyncCredentials {
	attrs := map[string]string{attrUsername: c.account.Username}
	if c.account.canSignIn() {
		attrs[attrLogin] = c.account.Login
		attrs[attrPassword] = c.account.Password
	}
	if c.session.CSRF != "" {
		attrs[attrCSRF] = c.session.CSRF
	}
	access := c.session.Current
	if access == "" {
		access = c.session.Remember
	}
	return &pluginv1.WatchSyncCredentials{
		AccessToken:      access,
		RefreshToken:     c.session.Remember,
		TokenType:        tokenType,
		SecretAttributes: attrs,
	}
}

func accountProto(username string) *pluginv1.WatchSyncAccount {
	return &pluginv1.WatchSyncAccount{
		ExternalSubject: strings.ToLower(username),
		Username:        username,
		DisplayName:     username,
		ProfileUrl:      letterboxd.DefaultBaseURL + "/" + strings.ToLower(username) + "/",
	}
}
