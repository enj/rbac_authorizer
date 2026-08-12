package ghapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// stubAuthorizer satisfies the client's credential requirement in tests that
// never reach a server.
func stubAuthorizer() Authorizer {
	return AuthorizerFunc(func(context.Context) (string, error) { return "Bearer stub", nil })
}

func TestParseBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		raw              string
		allowLoopback    bool
		want             string
		wantErrContains  string
		wantHiddenSecret string
	}{
		{name: "the production root is accepted", raw: DefaultBaseURL, want: "https://api.github.com"},
		{
			name: "an enterprise root keeps its path",
			raw:  "https://github.example.com/api/v3",
			want: "https://github.example.com/api/v3",
		},
		{
			name: "a trailing slash is normalized away",
			raw:  "https://github.example.com/api/v3/",
			want: "https://github.example.com/api/v3",
		},
		{
			name: "a bare trailing slash leaves no path",
			raw:  "https://api.github.com/",
			want: "https://api.github.com",
		},
		{
			name:            "plaintext is refused by default",
			raw:             "http://127.0.0.1:8080",
			wantErrContains: "must use https",
		},
		{
			name:          "plaintext loopback is allowed when asked for",
			raw:           "http://127.0.0.1:8080",
			allowLoopback: true,
			want:          "http://127.0.0.1:8080",
		},
		{
			name:            "plaintext to a routable host is refused even when asked for",
			raw:             "http://api.github.com",
			allowLoopback:   true,
			wantErrContains: "loopback",
		},
		{
			name:             "user information is refused",
			raw:              "https://soapbox:ghs_notarealtoken00000@api.github.com",
			wantErrContains:  "must not embed credentials",
			wantHiddenSecret: "ghs_notarealtoken00000",
		},
		{
			name:            "a query is refused",
			raw:             "https://api.github.com?access_token=x",
			wantErrContains: "must not carry a query",
		},
		{
			name:            "a fragment is refused",
			raw:             "https://api.github.com#top",
			wantErrContains: "must not carry a fragment",
		},
		{
			name:            "a missing host is refused",
			raw:             "https:///repos",
			wantErrContains: "must name a host",
		},
		{
			name:            "another scheme is refused",
			raw:             "ftp://api.github.com",
			wantErrContains: "must use https",
		},
		{
			name:            "an opaque URL is refused",
			raw:             "https:api.github.com",
			wantErrContains: "must not be opaque",
		},
		{
			name:            "a relative root is refused",
			raw:             "api.github.com/v3",
			wantErrContains: "must name a host",
		},
		{
			name: "a traversing base path is resolved by normalization",
			raw:  "https://github.example.com/api/../v3",
			want: "https://github.example.com/v3",
		},
		{
			name:            "a percent encoded traversal is refused",
			raw:             "https://github.example.com/api/%2e%2e/v3",
			wantErrContains: "must not traverse",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseBaseURL(test.raw, test.allowLoopback)
			if test.wantErrContains != "" {
				if err == nil {
					t.Fatalf("parseBaseURL(%q) succeeded as %q, want an error", test.raw, got)
				}
				if !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err, test.wantErrContains)
				}
				if test.wantHiddenSecret != "" && strings.Contains(err.Error(), test.wantHiddenSecret) {
					t.Fatalf("error %q echoes the secret", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBaseURL(%q): %v", test.raw, err)
			}
			if got.String() != test.want {
				t.Fatalf("parseBaseURL(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestResolveEscapesEverySegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		base            string
		segments        []string
		query           url.Values
		want            string
		wantErrContains string
	}{
		{
			name:     "segments join under the root",
			base:     DefaultBaseURL,
			segments: []string{"repos", "enj", "rbac_authorizer"},
			want:     "https://api.github.com/repos/enj/rbac_authorizer",
		},
		{
			name:     "an enterprise base path is kept in front",
			base:     "https://github.example.com/api/v3",
			segments: []string{"repos", "enj", "rbac_authorizer"},
			want:     "https://github.example.com/api/v3/repos/enj/rbac_authorizer",
		},
		{
			name:     "a space is percent encoded rather than sent raw",
			base:     DefaultBaseURL,
			segments: []string{"repos", "enj", "rbac authorizer"},
			want:     "https://api.github.com/repos/enj/rbac%20authorizer",
		},
		{
			name:     "a percent is escaped rather than reinterpreted",
			base:     DefaultBaseURL,
			segments: []string{"repos", "enj", "100%"},
			want:     "https://api.github.com/repos/enj/100%25",
		},
		{
			name:     "a query is encoded in sorted order",
			base:     DefaultBaseURL,
			segments: []string{"installation", "repositories"},
			query:    url.Values{"per_page": {"100"}, "page": {"2"}},
			want:     "https://api.github.com/installation/repositories?page=2&per_page=100",
		},
		{
			name:            "a separator in a segment is refused",
			base:            DefaultBaseURL,
			segments:        []string{"repos", "enj/other", "name"},
			wantErrContains: "URL separator",
		},
		{
			name:            "a query in a segment is refused",
			base:            DefaultBaseURL,
			segments:        []string{"repos", "enj?x=1", "name"},
			wantErrContains: "URL separator",
		},
		{
			name:            "traversal is refused",
			base:            DefaultBaseURL,
			segments:        []string{"repos", "..", "name"},
			wantErrContains: "must not traverse",
		},
		{
			name:            "an empty segment is refused",
			base:            DefaultBaseURL,
			segments:        []string{"repos", "", "name"},
			wantErrContains: "must not be empty",
		},
		{
			name:            "a control character is refused",
			base:            DefaultBaseURL,
			segments:        []string{"repos", "enj\nHost: evil", "name"},
			wantErrContains: "control characters",
		},
		{
			name:            "no path is refused",
			base:            DefaultBaseURL,
			wantErrContains: "needs a path",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			client, err := New(Config{Authorizer: stubAuthorizer(), BaseURL: test.base})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := client.resolve(test.segments, test.query)
			if test.wantErrContains != "" {
				if err == nil {
					t.Fatalf("resolve(%q) succeeded as %q, want an error", test.segments, got)
				}
				if !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err, test.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve(%q): %v", test.segments, err)
			}
			if got.String() != test.want {
				t.Fatalf("resolve(%q) = %q, want %q", test.segments, got, test.want)
			}
		})
	}
}

func TestHardenDoesNotMutateTheSuppliedClient(t *testing.T) {
	t.Parallel()

	base, err := parseBaseURL(DefaultBaseURL, false)
	if err != nil {
		t.Fatalf("parseBaseURL: %v", err)
	}
	supplied := &http.Client{Timeout: 7 * time.Second, Jar: stubJar{}}
	hardened := harden(supplied, base)

	if supplied.CheckRedirect != nil {
		t.Fatal("harden replaced the redirect policy of the caller's client")
	}
	if supplied.Jar == nil {
		t.Fatal("harden dropped the cookie jar of the caller's client")
	}
	if hardened.Jar != nil {
		t.Fatal("the hardened client kept a cookie jar")
	}
	if hardened.Timeout != supplied.Timeout {
		t.Fatalf("hardened timeout = %s, want %s", hardened.Timeout, supplied.Timeout)
	}
	if hardened.CheckRedirect == nil {
		t.Fatal("the hardened client has no redirect policy")
	}
}

// TestHardenBoundsTheDefaultClient covers the client this package builds for
// itself. Without a timeout it would wait forever on a destination that accepts
// the connection and then says nothing.
func TestHardenBoundsTheDefaultClient(t *testing.T) {
	t.Parallel()

	base, err := parseBaseURL(DefaultBaseURL, false)
	if err != nil {
		t.Fatalf("parseBaseURL: %v", err)
	}
	hardened := harden(nil, base)
	if hardened.Timeout != DefaultHTTPTimeout {
		t.Fatalf("default timeout = %s, want %s", hardened.Timeout, DefaultHTTPTimeout)
	}
	if hardened.Timeout <= 0 {
		t.Fatal("the default client is unbounded")
	}
}

func TestCanonicalOriginNormalizesCaseAndDefaultPorts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    string
		b    string
		same bool
	}{
		{name: "identical", a: "https://api.github.com", b: "https://api.github.com", same: true},
		{name: "host case differs", a: "https://api.github.com", b: "https://API.GitHub.Com", same: true},
		{name: "explicit default port", a: "https://api.github.com", b: "https://api.github.com:443", same: true},
		{name: "scheme case differs", a: "https://api.github.com", b: "HTTPS://api.github.com", same: true},
		{name: "explicit default http port", a: "http://127.0.0.1", b: "http://127.0.0.1:80", same: true},
		{name: "another host", a: "https://api.github.com", b: "https://evil.example", same: false},
		{name: "another port", a: "https://api.github.com", b: "https://api.github.com:8443", same: false},
		{name: "a downgraded scheme", a: "https://api.github.com", b: "http://api.github.com", same: false},
		{name: "a subdomain is not the origin", a: "https://api.github.com", b: "https://evil.api.github.com", same: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			first, err := url.Parse(test.a)
			if err != nil {
				t.Fatalf("parse %q: %v", test.a, err)
			}
			second, err := url.Parse(test.b)
			if err != nil {
				t.Fatalf("parse %q: %v", test.b, err)
			}
			if same := canonicalOrigin(first) == canonicalOrigin(second); same != test.same {
				t.Fatalf("%q and %q compared same=%t, want %t (%q vs %q)",
					test.a, test.b, same, test.same, canonicalOrigin(first), canonicalOrigin(second))
			}
		})
	}
}

func TestRetryAfterReadsBothForms(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{name: "absent", value: ""},
		{name: "seconds", value: "60", want: time.Minute, ok: true},
		{name: "seconds with padding", value: "  60  ", want: time.Minute, ok: true},
		{name: "zero seconds means retry now", value: "0", ok: true},
		{name: "negative seconds means retry now", value: "-5", ok: true},
		{
			name:  "an http date in the future",
			value: now.Add(90 * time.Second).Format(http.TimeFormat),
			want:  90 * time.Second,
			ok:    true,
		},
		{
			name:  "an http date in the past means retry now",
			value: now.Add(-time.Hour).Format(http.TimeFormat),
			ok:    true,
		},
		{name: "an unreadable value is reported as absent", value: "soon"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, ok := retryAfter(test.value, now)
			if ok != test.ok {
				t.Fatalf("retryAfter(%q) reported ok=%t, want %t", test.value, ok, test.ok)
			}
			if got != test.want {
				t.Fatalf("retryAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}

// stubJar is a cookie jar that is never used, only observed.
type stubJar struct{}

func (stubJar) SetCookies(*url.URL, []*http.Cookie) {}
func (stubJar) Cookies(*url.URL) []*http.Cookie     { return nil }
