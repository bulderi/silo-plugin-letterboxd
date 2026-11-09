package letterboxd

import (
	"context"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	http "github.com/bogdanfinn/fhttp"

	"github.com/bulderi/silo-plugin-letterboxd/internal/fakesite"
)

func TestRedirectsStayOnTheSite(t *testing.T) {
	base, err := url.Parse("https://letterboxd.example")
	if err != nil {
		t.Fatal(err)
	}
	check := stayOnSite(base)
	cases := []struct {
		name, target string
		via          int
		ok           bool
	}{
		{"same site", "https://letterboxd.example/film/some-film/", 1, true},
		{"plain http", "http://letterboxd.example/film/some-film/", 1, false},
		{"other host", "https://evil.example/", 1, false},
		{"subdomain", "https://a.letterboxd.example/", 1, false},
		{"too many", "https://letterboxd.example/film/some-film/", maxRedirects, false},
	}
	for _, tc := range cases {
		target, err := url.Parse(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		err = check(&http.Request{URL: target}, make([]*http.Request, tc.via))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", tc.name, err, tc.ok)
		}
	}
}

// The client must be built with the policy, not only have one defined.
func TestClientRefusesARedirectOffTheSite(t *testing.T) {
	hit := false
	other := httptest.NewServer(stdhttp.HandlerFunc(func(stdhttp.ResponseWriter, *stdhttp.Request) { hit = true }))
	defer other.Close()
	site := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		stdhttp.Redirect(w, r, other.URL+"/", stdhttp.StatusFound)
	}))
	defer site.Close()
	c, err := New(Options{BaseURL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.SetSession(Session{Remember: "remember"})
	if _, err := c.FilmByExternalID(context.Background(), "tmdb", "1"); err == nil {
		t.Fatal("want an error for a redirect to another site")
	}
	if hit {
		t.Fatal("the client followed a redirect to another site")
	}
}

func TestIsSignedInAsIgnoresCase(t *testing.T) {
	site := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		_, _ = w.Write([]byte(`<input name="username" value="SampleMember">`))
	}))
	defer site.Close()
	c, err := New(Options{BaseURL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	for username, want := range map[string]bool{"samplemember": true, "SAMPLEMEMBER": true, "someoneelse": false} {
		got, err := c.IsSignedInAs(context.Background(), username)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", username, got, err, want)
		}
	}
}

func TestSessionCanWrite(t *testing.T) {
	site := fakesite.New(t)
	c, err := New(Options{BaseURL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.SetSession(Session{Remember: site.IssueRememberCookie()})
	if ok, err := c.SessionCanWrite(context.Background()); err != nil || !ok {
		t.Fatalf("live cookie: %v, %v", ok, err)
	}
	site.ExpireSessions()
	if ok, err := c.SessionCanWrite(context.Background()); err != nil || ok {
		t.Fatalf("dead cookie: %v, %v", ok, err)
	}
	if len(site.Watchlist()) != 0 {
		t.Fatalf("the check changed the watchlist: %v", site.Watchlist())
	}
}

func TestParseRememberCookie(t *testing.T) {
	cases := []struct {
		name, input, want string
		ok                bool
	}{
		{"value", "abc123", "abc123", true},
		{"value with padding", "abc123==", "abc123==", true},
		{"surrounding space", "  abc123\n", "abc123", true},
		{"name and value", "letterboxd.user=abc123", "abc123", true},
		{"empty", "   ", "", false},
		{"name only", "letterboxd.user=", "", false},
		{"session cookie", "letterboxd.user.CURRENT=abc123", "", false},
		{"cookie header", "a=b; letterboxd.user=abc123", "", false},
		{"inner space", "my pass phrase", "", false},
		{"non-ascii", "lösenord", "", false},
	}
	for _, tc := range cases {
		got, ok := ParseRememberCookie(tc.input)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
