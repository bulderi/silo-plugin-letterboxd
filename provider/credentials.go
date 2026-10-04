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

	attrUsername = "username"
	attrLogin    = "login"
	attrPassword = "password"
	attrCSRF     = "csrf"
	tokenType    = "letterboxd-session"
)

// account is what the plugin needs to act for one Letterboxd member. The host
// stores it encrypted in the connection's credential bundle.
type account struct {
	// Username is the member's canonical username as the site reports it
	// after sign-in, which is what watchlist URLs use.
	Username string
	// Login is what the user typed to sign in (username or email).
	Login    string
	Password string
}

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
	if acc.Username == "" || acc.Password == "" {
		return credentials{}, false
	}
	return credentials{
		account: acc,
		session: letterboxd.Session{
			Current:  stored.GetAccessToken(),
			Remember: stored.GetRefreshToken(),
			CSRF:     attrs[attrCSRF],
		},
	}, true
}

// proto builds the bundle the host persists. The access token is the session
// cookie; the refresh token is the remember-me cookie.
func (c credentials) proto() *pluginv1.WatchSyncCredentials {
	attrs := map[string]string{
		attrUsername: c.account.Username,
		attrLogin:    c.account.Login,
		attrPassword: c.account.Password,
	}
	if c.session.CSRF != "" {
		attrs[attrCSRF] = c.session.CSRF
	}
	return &pluginv1.WatchSyncCredentials{
		AccessToken:      c.session.Current,
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
