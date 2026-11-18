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
	tokenType    = "letterboxd-session"
)

// credentials is the stored form of a connection: the member's username,
// which is what list URLs use, and the session cookies. The host stores it
// encrypted in the connection's credential bundle.
type credentials struct {
	username string
	session  letterboxd.Session
}

// credentialsFromContext reads the stored credential bundle. ok is false when
// the bundle is missing the username or the cookies, which means the
// connection predates the plugin or was damaged; the user has to reconnect.
func credentialsFromContext(ctx *pluginv1.WatchSyncAuthenticatedContext) (credentials, bool) {
	stored := ctx.GetCredentials()
	attrs := stored.GetSecretAttributes()
	username := strings.TrimSpace(attrs[attrUsername])
	current, remember := stored.GetAccessToken(), stored.GetRefreshToken()
	if current == remember {
		// proto stored the remember-me cookie twice; there is no session cookie.
		current = ""
	}
	if username == "" || (current == "" && remember == "") {
		return credentials{}, false
	}
	return credentials{
		username: username,
		session: letterboxd.Session{
			Current:  current,
			Remember: remember,
		},
	}, true
}

// proto builds the bundle the host persists. The access token is the session
// cookie and the refresh token the remember-me cookie. Silo refuses a bundle
// without an access token and the site sets no session cookie for a request
// carrying only the remember-me cookie, so that cookie then fills both fields.
func (c credentials) proto() *pluginv1.WatchSyncCredentials {
	attrs := map[string]string{attrUsername: c.username}
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
