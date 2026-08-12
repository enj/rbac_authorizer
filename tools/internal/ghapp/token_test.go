package ghapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/ghapi"
	"github.com/enj/soapbox/tools/internal/ghapp"
)

// Installation tokens the stand-in mints, in the order it mints them. Nothing
// this package renders may contain either value.
const (
	firstToken  = "ghs_notarealtoken0000000001"
	secondToken = "ghs_notarealtoken0000000002"
)

// testStart is the instant every test clock begins at.
var testStart = time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

// testClock is a hand advanced clock, so expiry and renewal are exact.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// newClock starts a clock at testStart.
func newClock() *testClock { return &testClock{now: testStart} }

// Now reports the current instant.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// advance moves the clock forward.
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// mintServer stands in for the installation token endpoint.
type mintServer struct {
	server *httptest.Server
	clock  *testClock

	// reply answers the nth mint, counted from one. A nil reply mints a token
	// that satisfies the default test configuration.
	reply func(mint int) (int, any)

	// delay is held before each answer, so concurrent callers pile up.
	delay time.Duration

	mu       sync.Mutex
	mints    int
	paths    []string
	auth     []string
	bodies   [][]byte
	accepts  []string
	versions []string
}

// newMintServer starts a token endpoint.
func newMintServer(t *testing.T, clock *testClock) *mintServer {
	t.Helper()
	stub := &mintServer{clock: clock}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		stub.mu.Lock()
		stub.mints++
		mint := stub.mints
		stub.paths = append(stub.paths, r.URL.EscapedPath())
		stub.auth = append(stub.auth, r.Header.Get("Authorization"))
		stub.accepts = append(stub.accepts, r.Header.Get("Accept"))
		stub.versions = append(stub.versions, r.Header.Get("X-GitHub-Api-Version"))
		stub.bodies = append(stub.bodies, body)
		reply, delay := stub.reply, stub.delay
		stub.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		status, payload := http.StatusCreated, any(stub.defaultToken(mint))
		if reply != nil {
			status, payload = reply(mint)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// defaultToken is a grant that satisfies the default test configuration, always
// expiring an hour after whatever the clock currently reads.
func (m *mintServer) defaultToken(mint int) map[string]any {
	token := firstToken
	if mint > 1 {
		token = secondToken
	}
	return map[string]any{
		"token":                token,
		"expires_at":           m.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"permissions":          map[string]string{"contents": "write", "issues": "write"},
		"repository_selection": "selected",
		"repositories":         []map[string]any{{"name": "rbac_authorizer", "full_name": "enj/rbac_authorizer"}},
	}
}

// count reports how many tokens were minted.
func (m *mintServer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mints
}

// recorded reports what the endpoint received.
func (m *mintServer) recorded() (paths, auth, accepts, versions []string, bodies [][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.paths...), append([]string(nil), m.auth...),
		append([]string(nil), m.accepts...), append([]string(nil), m.versions...),
		append([][]byte(nil), m.bodies...)
}

// app builds an App pointed at the stand-in.
func (m *mintServer) app(t *testing.T, adjust func(*ghapp.Config)) *ghapp.App {
	t.Helper()
	cfg := testConfig(t)
	cfg.BaseURL = m.server.URL
	cfg.AllowPlaintextLoopback = true
	cfg.Clock = m.clock.Now
	if adjust != nil {
		adjust(&cfg)
	}
	app, err := ghapp.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return app
}

func TestTokenIsMintedWithAnAppJWTAndANarrowedRequest(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	app := stub.app(t, nil)

	header, err := app.AuthorizationHeader(t.Context())
	if err != nil {
		t.Fatalf("AuthorizationHeader: %v", err)
	}
	if header != "Bearer "+firstToken {
		t.Fatalf("header = %q, want the minted token", header)
	}

	paths, auth, accepts, versions, bodies := stub.recorded()
	if len(paths) != 1 {
		t.Fatalf("minted %d times, want once", len(paths))
	}
	if paths[0] != "/app/installations/7/access_tokens" {
		t.Errorf("path = %q, want /app/installations/7/access_tokens", paths[0])
	}
	if accepts[0] != "application/vnd.github+json" {
		t.Errorf("accept = %q", accepts[0])
	}
	if versions[0] != "2022-11-28" {
		t.Errorf("api version = %q", versions[0])
	}
	const wantBody = `{"repositories":["rbac_authorizer"],"permissions":{"contents":"write"}}`
	if body := strings.TrimSpace(string(bodies[0])); body != wantBody {
		t.Errorf("body = %s, want %s", body, wantBody)
	}

	// The credential presented to the token endpoint is an App JWT, not a
	// token, and it must be exactly the claim set GitHub requires.
	presented, ok := strings.CutPrefix(auth[0], "Bearer ")
	if !ok {
		t.Fatalf("authorization = %q, want a bearer credential", auth[0])
	}
	rawHeader, claims := splitJWT(t, presented, &testKey(t).PublicKey)
	if rawHeader != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("jose header = %s", rawHeader)
	}
	if claims.Issuer != "42" {
		t.Errorf("iss = %q, want the app id", claims.Issuer)
	}
	if want := testStart.Add(-60 * time.Second).Unix(); claims.IssuedAt != want {
		t.Errorf("iat = %d, want %d, one minute behind the clock", claims.IssuedAt, want)
	}
	if age := claims.ExpiresAt - claims.IssuedAt; age > int64((10 * time.Minute).Seconds()) {
		t.Errorf("exp - iat = %d seconds, GitHub refuses anything past 600", age)
	}
	if claims.ExpiresAt <= testStart.Unix() {
		t.Errorf("exp = %d is not in the future of %d", claims.ExpiresAt, testStart.Unix())
	}
}

func TestAppJWTIsDeterministic(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)

	// Two Apps holding the same key and reading the same clock must present
	// byte identical credentials: RS256 is deterministic, so a difference would
	// mean something in the claim set is not.
	for range 2 {
		if _, err := stub.app(t, nil).AuthorizationHeader(t.Context()); err != nil {
			t.Fatalf("AuthorizationHeader: %v", err)
		}
	}
	_, auth, _, _, _ := stub.recorded()
	if len(auth) != 2 {
		t.Fatalf("minted %d times, want twice", len(auth))
	}
	if auth[0] != auth[1] {
		t.Fatal("two mints at the same instant produced different JWTs")
	}
}

func TestCachedTokenIsReusedUntilTheRenewalMargin(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	app := stub.app(t, nil)

	tests := []struct {
		name      string
		advance   time.Duration
		wantMints int
		wantToken string
	}{
		{name: "the first call mints", wantMints: 1, wantToken: firstToken},
		{name: "a later call reuses", advance: 30 * time.Minute, wantMints: 1, wantToken: firstToken},
		{name: "just outside the margin still reuses", advance: 24*time.Minute + 59*time.Second, wantMints: 1, wantToken: firstToken},
		{name: "inside the margin renews", advance: 2 * time.Second, wantMints: 2, wantToken: secondToken},
		{name: "the renewed token is then reused", advance: time.Minute, wantMints: 2, wantToken: secondToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock.advance(test.advance)
			header, err := app.AuthorizationHeader(t.Context())
			if err != nil {
				t.Fatalf("AuthorizationHeader: %v", err)
			}
			if header != "Bearer "+test.wantToken {
				t.Errorf("header = %q, want %q", header, "Bearer "+test.wantToken)
			}
			if got := stub.count(); got != test.wantMints {
				t.Errorf("minted %d times, want %d", got, test.wantMints)
			}
		})
	}
}

func TestConcurrentCallersMintOnce(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	// The delay makes every caller arrive while the first mint is in flight, so
	// the test fails if the gate is not doing its job.
	stub.delay = 25 * time.Millisecond
	app := stub.app(t, nil)

	const callers = 16
	headers := make([]string, callers)
	failures := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			headers[i], failures[i] = app.AuthorizationHeader(t.Context())
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range failures {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if headers[i] != "Bearer "+firstToken {
			t.Fatalf("caller %d got %q, want the one minted token", i, headers[i])
		}
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("%d callers minted %d tokens, want 1", callers, got)
	}
}

func TestInstallationReportsTheGrantWithoutTheCredential(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	app := stub.app(t, nil)

	installation, err := app.Installation(t.Context())
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if installation.ID != testInstallationID {
		t.Errorf("id = %d, want %d", installation.ID, testInstallationID)
	}
	if want := testStart.Add(time.Hour); !installation.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %s, want %s", installation.ExpiresAt, want)
	}
	if installation.RepositorySelection != "selected" {
		t.Errorf("selection = %q, want selected", installation.RepositorySelection)
	}
	if got := installation.Repositories; len(got) != 1 || got[0] != "enj/rbac_authorizer" {
		t.Errorf("repositories = %v", got)
	}
	if got := installation.Permissions["contents"]; got != "write" {
		t.Errorf("contents permission = %q, want write", got)
	}

	// The returned grant is a copy: mutating it must not reach the cache.
	installation.Permissions["contents"] = "admin"
	installation.Repositories[0] = "attacker/repo"
	again, err := app.Installation(t.Context())
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if again.Permissions["contents"] != "write" || again.Repositories[0] != "enj/rbac_authorizer" {
		t.Fatalf("the cached grant was mutated through a returned copy: %+v", again)
	}
}

func TestMintedGrantIsVerified(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		token           map[string]any
		wantErr         error
		wantErrContains string
	}{
		{
			name:            "an empty token is refused",
			token:           map[string]any{"token": "", "expires_at": testStart.Add(time.Hour).Format(time.RFC3339)},
			wantErrContains: "empty token",
		},
		{
			name:            "a token with no expiry is refused",
			token:           map[string]any{"token": firstToken},
			wantErrContains: "no expiry",
		},
		{
			name: "a token already inside the renewal margin is refused",
			token: map[string]any{
				"token":      firstToken,
				"expires_at": testStart.Add(2 * time.Minute).Format(time.RFC3339),
			},
			wantErrContains: "renewal margin",
		},
		{
			name: "a missing permission is refused",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"issues": "write"},
				"repository_selection": "selected",
				"repositories":         []map[string]any{{"full_name": "enj/rbac_authorizer"}},
			},
			wantErr: ghapp.ErrInsufficientPermission,
		},
		{
			name: "a weaker permission than required is refused",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"contents": "read"},
				"repository_selection": "selected",
				"repositories":         []map[string]any{{"full_name": "enj/rbac_authorizer"}},
			},
			wantErr: ghapp.ErrInsufficientPermission,
		},
		{
			name: "a stronger permission than required is accepted",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"contents": "admin"},
				"repository_selection": "selected",
				"repositories":         []map[string]any{{"full_name": "enj/rbac_authorizer"}},
			},
		},
		{
			name: "a token that misses a required repository is refused",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"contents": "write"},
				"repository_selection": "selected",
				"repositories":         []map[string]any{{"full_name": "enj/something_else"}},
			},
			wantErr: ghapp.ErrRepositoryScope,
		},
		{
			name: "a token wider than the request is refused",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"contents": "write"},
				"repository_selection": "all",
			},
			wantErr: ghapp.ErrRepositoryScope,
		},
		{
			name: "a selected token reaching more than was requested is refused",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"contents": "write"},
				"repository_selection": "selected",
				"repositories": []map[string]any{
					{"full_name": "enj/rbac_authorizer"},
					{"full_name": "enj/private_notes"},
				},
			},
			wantErr:         ghapp.ErrRepositoryScope,
			wantErrContains: "also reaches",
		},
		{
			name: "repository names are matched case insensitively",
			token: map[string]any{
				"token":                firstToken,
				"expires_at":           testStart.Add(time.Hour).Format(time.RFC3339),
				"permissions":          map[string]string{"contents": "write"},
				"repository_selection": "Selected",
				"repositories":         []map[string]any{{"full_name": "ENJ/RBAC_Authorizer"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			clock := newClock()
			stub := newMintServer(t, clock)
			stub.reply = func(int) (int, any) { return http.StatusCreated, test.token }
			app := stub.app(t, nil)

			_, err := app.AuthorizationHeader(t.Context())
			if test.wantErr == nil && test.wantErrContains == "" {
				if err != nil {
					t.Fatalf("AuthorizationHeader: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the grant was accepted, want an error")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error %q is not %v", err, test.wantErr)
			}
			if test.wantErrContains != "" && !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("error %q does not contain %q", err, test.wantErrContains)
			}
			// Every refusal above describes a token that GitHub already
			// minted, so none of them may quote it.
			if strings.Contains(err.Error(), firstToken) {
				t.Fatalf("error %q carries the token", err)
			}
		})
	}
}

// TestAnUnnarrowedInstallationAcceptsAWideToken covers the configuration a run
// uses before it knows its destinations: nothing is requested, so nothing is
// required, and the account wide token GitHub answers with is the correct one.
func TestAnUnnarrowedInstallationAcceptsAWideToken(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	stub.reply = func(int) (int, any) {
		return http.StatusCreated, map[string]any{
			"token":                firstToken,
			"expires_at":           clock.Now().Add(time.Hour).Format(time.RFC3339),
			"permissions":          map[string]string{"contents": "read"},
			"repository_selection": "all",
		}
	}
	app := stub.app(t, func(c *ghapp.Config) {
		c.Repositories = nil
		c.Permissions = nil
	})

	installation, err := app.Installation(t.Context())
	if err != nil {
		t.Fatalf("Installation: %v", err)
	}
	if installation.RepositorySelection != "all" {
		t.Fatalf("selection = %q, want all", installation.RepositorySelection)
	}
	if len(installation.Repositories) != 0 {
		t.Fatalf("repositories = %v, want none listed", installation.Repositories)
	}
	// An empty narrowing request is sent as an empty object rather than as a
	// request for something the installation may not hold.
	_, _, _, _, bodies := stub.recorded()
	if body := strings.TrimSpace(string(bodies[0])); body != "{}" {
		t.Fatalf("body = %s, want {}", body)
	}
}

// TestATerminalRefusalIsNotMintedAgain covers the mint storm a permanently
// misconfigured installation would otherwise cause. The refusal is a function
// of this App's configuration and the installation's grant, so a retry reaches
// the same answer, and doing that per request would spend a live token each
// time against the installation's budget.
func TestATerminalRefusalIsNotMintedAgain(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	stub.reply = func(int) (int, any) {
		return http.StatusCreated, map[string]any{
			"token":                firstToken,
			"expires_at":           clock.Now().Add(time.Hour).Format(time.RFC3339),
			"permissions":          map[string]string{"contents": "read"},
			"repository_selection": "selected",
			"repositories":         []map[string]any{{"full_name": "enj/rbac_authorizer"}},
		}
	}
	app := stub.app(t, nil)

	var first string
	for i := range 5 {
		_, err := app.AuthorizationHeader(t.Context())
		if !errors.Is(err, ghapp.ErrInsufficientPermission) {
			t.Fatalf("call %d: error = %v, want ErrInsufficientPermission", i, err)
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("call %d reported %q, want the same refusal as %q", i, err, first)
		}
	}

	// The other entry points share the refusal rather than minting for
	// themselves, and the callback never runs with a grant that was refused.
	if _, err := app.Installation(t.Context()); !errors.Is(err, ghapp.ErrInsufficientPermission) {
		t.Fatalf("Installation: error = %v, want ErrInsufficientPermission", err)
	}
	err := app.WithCredential(t.Context(), func(string) error {
		t.Error("the callback ran with a refused grant")
		return nil
	})
	if !errors.Is(err, ghapp.ErrInsufficientPermission) {
		t.Fatalf("WithCredential: error = %v, want ErrInsufficientPermission", err)
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("minted %d tokens for one permanent refusal, want 1", got)
	}

	// Time passing does not make the grant acceptable, so the expiry of the
	// token that failed must not restart the minting either.
	clock.advance(2 * time.Hour)
	if _, err := app.AuthorizationHeader(t.Context()); !errors.Is(err, ghapp.ErrInsufficientPermission) {
		t.Fatalf("after expiry: error = %v, want ErrInsufficientPermission", err)
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("minted %d tokens after the failed token expired, want 1", got)
	}
	if strings.Contains(first, firstToken) {
		t.Fatalf("refusal %q carries the token", first)
	}
}

func TestConcurrentCallersShareATerminalRefusal(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	stub.delay = 25 * time.Millisecond
	stub.reply = func(int) (int, any) {
		return http.StatusCreated, map[string]any{
			"token":                firstToken,
			"expires_at":           clock.Now().Add(time.Hour).Format(time.RFC3339),
			"permissions":          map[string]string{"contents": "read"},
			"repository_selection": "selected",
			"repositories":         []map[string]any{{"full_name": "enj/rbac_authorizer"}},
		}
	}
	app := stub.app(t, nil)

	const callers = 16
	failures := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, failures[i] = app.AuthorizationHeader(t.Context())
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range failures {
		if !errors.Is(err, ghapp.ErrInsufficientPermission) {
			t.Fatalf("caller %d: error = %v, want ErrInsufficientPermission", i, err)
		}
	}
	if got := stub.count(); got != 1 {
		t.Fatalf("%d callers minted %d tokens, want 1", callers, got)
	}
}

// TestATransientRefusalIsRetried is the other half of the rule above: only a
// verified grant is terminal. A refusal from the transport or from a 5xx says
// nothing about whether the installation is configured correctly, so caching it
// would strand a run that would have succeeded a moment later.
func TestATransientRefusalIsRetried(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	stub.reply = func(mint int) (int, any) {
		if mint == 1 {
			return http.StatusInternalServerError, map[string]any{"message": "Server Error"}
		}
		return http.StatusCreated, stub.defaultToken(mint)
	}
	app := stub.app(t, nil)

	if _, err := app.AuthorizationHeader(t.Context()); err == nil {
		t.Fatal("the server error was reported as success")
	}
	header, err := app.AuthorizationHeader(t.Context())
	if err != nil {
		t.Fatalf("the retry after a server error: %v", err)
	}
	if header != "Bearer "+secondToken {
		t.Fatalf("header = %q, want the token from the second mint", header)
	}
	if got := stub.count(); got != 2 {
		t.Fatalf("minted %d times, want 2", got)
	}
}

func TestGitHubRefusalsSurfaceTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    map[string]any
		wantErr error
	}{
		{
			name:    "an app that is not installed",
			status:  http.StatusNotFound,
			body:    map[string]any{"message": "Not Found"},
			wantErr: ghapi.ErrNotFound,
		},
		{
			name:    "a key that GitHub does not recognise",
			status:  http.StatusUnauthorized,
			body:    map[string]any{"message": "A JSON web token could not be decoded"},
			wantErr: ghapi.ErrUnauthorized,
		},
		{
			name:    "permissions the installation does not hold",
			status:  http.StatusUnprocessableEntity,
			body:    map[string]any{"message": "The permissions requested are not granted to this installation."},
			wantErr: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			clock := newClock()
			stub := newMintServer(t, clock)
			stub.reply = func(int) (int, any) { return test.status, test.body }

			_, err := stub.app(t, nil).AuthorizationHeader(t.Context())
			if err == nil {
				t.Fatal("the refusal was reported as success")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error %q is not %v", err, test.wantErr)
			}
			var failure *ghapi.StatusError
			if !errors.As(err, &failure) {
				t.Fatalf("error %q is not a *ghapi.StatusError", err)
			}
			if failure.Status != test.status {
				t.Fatalf("status = %d, want %d", failure.Status, test.status)
			}
			if !strings.Contains(err.Error(), "mint installation token") {
				t.Fatalf("error %q does not say what failed", err)
			}
		})
	}
}

func TestSecretsNeverReachErrorsOrReports(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	// The grant is verified after the token exists, so this refusal is raised
	// while the package is holding a live credential.
	stub.reply = func(int) (int, any) {
		return http.StatusCreated, map[string]any{
			"token":                firstToken,
			"expires_at":           clock.Now().Add(time.Hour).Format(time.RFC3339),
			"permissions":          map[string]string{"contents": "read"},
			"repository_selection": "selected",
			"repositories":         []map[string]any{{"full_name": "enj/rbac_authorizer"}},
		}
	}
	app := stub.app(t, nil)

	_, err := app.AuthorizationHeader(t.Context())
	if !errors.Is(err, ghapp.ErrInsufficientPermission) {
		t.Fatalf("error = %v, want ErrInsufficientPermission", err)
	}
	if strings.Contains(err.Error(), firstToken) {
		t.Fatalf("error %q carries the token", err)
	}
	// The redactor is seeded before the refusal is raised, so a report written
	// afterwards is scrubbed even though the run never got a usable token.
	redacted := app.Redactor().String("a report mentioning " + firstToken)
	if strings.Contains(redacted, firstToken) {
		t.Fatalf("the redactor let the token through: %q", redacted)
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("the redactor did not mark the removal: %q", redacted)
	}
}

func TestRedactorCoversEveryMintedToken(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	app := stub.app(t, nil)

	if _, err := app.AuthorizationHeader(t.Context()); err != nil {
		t.Fatalf("AuthorizationHeader: %v", err)
	}
	clock.advance(56 * time.Minute)
	if _, err := app.AuthorizationHeader(t.Context()); err != nil {
		t.Fatalf("AuthorizationHeader after renewal: %v", err)
	}
	if got := stub.count(); got != 2 {
		t.Fatalf("minted %d times, want 2", got)
	}

	// A renewal does not make the replaced token safe to print.
	scrubbed := app.Redactor().String(firstToken + " then " + secondToken)
	if strings.Contains(scrubbed, firstToken) || strings.Contains(scrubbed, secondToken) {
		t.Fatalf("the redactor let a token through: %q", scrubbed)
	}
}

func TestWithCredentialHandsOutTheTokenAndRedactsWhatComesBack(t *testing.T) {
	t.Parallel()

	clock := newClock()
	stub := newMintServer(t, clock)
	app := stub.app(t, nil)

	var seen string
	if err := app.WithCredential(t.Context(), func(token string) error {
		seen = token
		return nil
	}); err != nil {
		t.Fatalf("WithCredential: %v", err)
	}
	if seen != firstToken {
		t.Fatalf("callback saw %q, want the minted token", seen)
	}

	// A caller that puts the credential into its own error is the most likely
	// way one escapes, so the way back out is scrubbed too.
	err := app.WithCredential(t.Context(), func(token string) error {
		return errors.New("git push failed with " + token)
	})
	if err == nil {
		t.Fatal("the callback error was dropped")
	}
	if strings.Contains(err.Error(), firstToken) {
		t.Fatalf("error %q carries the token", err)
	}
	if !strings.Contains(err.Error(), "git push failed") {
		t.Fatalf("error %q lost the callback's message", err)
	}

	if err := app.WithCredential(t.Context(), nil); err == nil {
		t.Fatal("a nil callback was accepted")
	}
}

func TestCancellationStopsMinting(t *testing.T) {
	t.Parallel()

	t.Run("before the request", func(t *testing.T) {
		t.Parallel()

		clock := newClock()
		stub := newMintServer(t, clock)
		app := stub.app(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := app.AuthorizationHeader(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if got := stub.count(); got != 0 {
			t.Fatalf("minted %d times with a cancelled context, want 0", got)
		}
	})

	t.Run("while waiting behind another caller", func(t *testing.T) {
		t.Parallel()

		clock := newClock()
		stub := newMintServer(t, clock)
		stub.delay = 250 * time.Millisecond
		app := stub.app(t, nil)

		// The first caller holds the gate for the length of the delay. The
		// second must give up on its own context rather than wait it out.
		first := make(chan error, 1)
		go func() {
			_, err := app.AuthorizationHeader(t.Context())
			first <- err
		}()
		time.Sleep(25 * time.Millisecond)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := app.AuthorizationHeader(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if err := <-first; err != nil {
			t.Fatalf("the caller holding the gate: %v", err)
		}
	})
}
