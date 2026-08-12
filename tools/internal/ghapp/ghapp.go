// Package ghapp is the GitHub App credential boundary of the engine.
//
// It turns an App private key into short lived installation tokens and hands
// them out through two narrow accessors: an Authorization header for the typed
// REST client, and a callback for the Git boundary, which needs the bare value
// to place in a subprocess environment. There is no accessor that simply
// returns the token, because a credential with no scope around it is a
// credential that ends up in a log.
//
// A minted token is checked before it is used. GitHub is asked for a token
// narrowed to the repositories and permissions the run actually needs, and the
// grant it answers with is verified against that request, so a token that is
// broader or narrower than expected ends the run rather than quietly changing
// what it can do. The exact token value seeds a redactor before anything else
// reads the response, so no error raised after minting can carry it.
//
// The token request is made through internal/ghapi, so the base URL rules,
// redirect refusal, response bound, and typed errors of the REST boundary cover
// the credential endpoint as well as every endpoint that uses its output.
package ghapp

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/enj/soapbox/tools/internal/ghapi"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// DefaultRenewBefore is how long before expiry a cached token is replaced.
//
// GitHub mints installation tokens with a one hour life. The margin covers the
// longest single operation a run performs with one token, so a push cannot
// start with a token that expires while it is in flight.
const DefaultRenewBefore = 5 * time.Minute

// maxRenewBefore bounds the margin. A margin at or past the token lifetime
// would make every token stale on arrival and turn every call into a mint.
const maxRenewBefore = 30 * time.Minute

// DefaultHTTPTimeout bounds one REST call when the caller supplies no client.
// It is the REST boundary's own default, named here so a caller configuring an
// App can see the bound without reading another package.
const DefaultHTTPTimeout = ghapi.DefaultHTTPTimeout

// Permission levels GitHub grants, ordered so a grant can be compared with a
// requirement rather than only matched against it.
var permissionLevels = map[string]int{"none": 0, "read": 1, "write": 2, "admin": 3}

// Refusals a caller can branch on with errors.Is when a minted token does not
// carry the grant the run needs. Both mean the App's installation was changed
// on GitHub, so both are configuration failures rather than transient ones.
var (
	// ErrInsufficientPermission reports a token missing a required permission,
	// or holding it at a weaker level than required.
	ErrInsufficientPermission = errors.New("the installation token lacks a required permission")

	// ErrRepositoryScope reports a token that does not reach a required
	// repository, or that reaches more than was requested.
	ErrRepositoryScope = errors.New("the installation token does not carry the requested repository scope")
)

// Clock reports the current time.
//
// It is injected so token expiry, renewal, and JWT claims are exact in tests
// rather than approximate. Nothing in this package reads the wall clock
// directly.
type Clock func() time.Time

// Config describes one GitHub App installation.
type Config struct {
	// AppID is the numeric App identifier GitHub issued. It is the JWT issuer.
	//
	// It is an int64 rather than a string because GitHub only ever issues a
	// number here, and a typed field makes the one wrong value that matters,
	// something that is not a number at all, unrepresentable.
	AppID int64

	// InstallationID identifies the installation tokens are minted for.
	InstallationID int64

	// PrivateKeyPEM is the App's RSA private key in PEM form, in either PKCS#1
	// or PKCS#8 encoding. New parses it and keeps only the parsed key, so the
	// caller owns the lifetime of these bytes.
	PrivateKeyPEM []byte

	// Repositories are the repositories the run needs, as owner/name. They
	// narrow the minted token and are then verified against the grant GitHub
	// answers with. An empty list mints a token carrying the installation's
	// full repository selection, which is only correct for a run that does not
	// know its destinations yet.
	Repositories []string

	// Permissions are the permissions the run needs, mapping a permission name
	// to read, write, or admin. They narrow the minted token the same way, and
	// a grant weaker than one of them is refused.
	Permissions map[string]string

	// BaseURL is the REST API root. It defaults to ghapi.DefaultBaseURL.
	BaseURL string

	// HTTPClient is the transport. It defaults to a client bounded by
	// DefaultHTTPTimeout.
	HTTPClient *http.Client

	// UserAgent defaults to ghapi.DefaultUserAgent.
	UserAgent string

	// Clock defaults to time.Now.
	Clock Clock

	// RenewBefore defaults to DefaultRenewBefore.
	RenewBefore time.Duration

	// AllowPlaintextLoopback permits an http base URL naming a loopback
	// address, for httptest servers and nothing else.
	AllowPlaintextLoopback bool
}

// App mints and holds one installation's credentials.
//
// A single App is safe for concurrent use and is meant to be shared: callers
// that each built their own would each mint their own token, and GitHub counts
// those against the installation.
type App struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	clock          Clock
	renewBefore    time.Duration
	client         *ghapi.Client

	// want is the grant this App requires, and names is the same repository
	// list reduced to the bare names GitHub's token request expects.
	want  Installation
	names []string

	// gate serializes minting. It is a channel rather than a mutex so a caller
	// waiting behind another caller's mint can still honour its own context.
	gate chan struct{}

	// mu guards the cached credential. It is only ever held for field
	// assignments, never across a request.
	mu       sync.Mutex
	token    string
	current  Installation
	secrets  []string
	redactor *gitcli.Redactor

	// verifyFailure is the terminal refusal of a grant GitHub already minted.
	// It is remembered because it cannot come out differently on a retry, and
	// minting again to be told the same thing would spend a live token per
	// request against the installation's budget.
	verifyFailure error
}

// New builds an App from cfg. It performs no I/O: the key is parsed and every
// value is checked, so a misconfigured App fails at construction rather than at
// the first push.
func New(cfg Config) (*App, error) {
	if cfg.AppID <= 0 {
		return nil, fmt.Errorf("github app: app id %d must be positive", cfg.AppID)
	}
	if cfg.InstallationID <= 0 {
		return nil, fmt.Errorf("github app: installation id %d must be positive", cfg.InstallationID)
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app: %w", err)
	}
	repositories, names, err := splitRepositories(cfg.Repositories)
	if err != nil {
		return nil, fmt.Errorf("github app: %w", err)
	}
	permissions, err := checkedPermissions(cfg.Permissions)
	if err != nil {
		return nil, fmt.Errorf("github app: %w", err)
	}
	renewBefore := cfg.RenewBefore
	if renewBefore == 0 {
		renewBefore = DefaultRenewBefore
	}
	if renewBefore < 0 || renewBefore > maxRenewBefore {
		return nil, fmt.Errorf("github app: renewal margin %s must be between 0 and %s", cfg.RenewBefore, maxRenewBefore)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	app := &App{
		appID:          cfg.AppID,
		installationID: cfg.InstallationID,
		key:            key,
		clock:          clock,
		renewBefore:    renewBefore,
		want:           Installation{Permissions: permissions, Repositories: repositories},
		names:          names,
		gate:           make(chan struct{}, 1),
		redactor:       gitcli.NewRedactor(),
	}
	// The client this App mints through presents an App JWT rather than an
	// installation token, so its authorizer is the one credential that does not
	// depend on the token this App is trying to obtain.
	client, err := ghapi.New(ghapi.Config{
		Authorizer:             ghapi.AuthorizerFunc(app.jwtHeader),
		BaseURL:                cfg.BaseURL,
		HTTPClient:             cfg.HTTPClient,
		UserAgent:              cfg.UserAgent,
		AllowPlaintextLoopback: cfg.AllowPlaintextLoopback,
	})
	if err != nil {
		return nil, fmt.Errorf("github app: %w", err)
	}
	app.client = client
	return app, nil
}

// BaseURL reports the REST origin this App mints against.
func (a *App) BaseURL() string { return a.client.BaseURL() }

// splitRepositories checks owner/name entries and reduces them to bare names.
//
// One installation belongs to one account, so entries that name two owners
// describe an installation that cannot exist and are refused here rather than
// by GitHub after a token has already been minted.
func splitRepositories(entries []string) (full []string, names []string, err error) {
	if len(entries) == 0 {
		return nil, nil, nil
	}
	owner := ""
	seen := make(map[string]bool, len(entries))
	full = make([]string, 0, len(entries))
	names = make([]string, 0, len(entries))
	for _, entry := range entries {
		entryOwner, entryName, ok := strings.Cut(entry, "/")
		if !ok || strings.Contains(entryName, "/") {
			return nil, nil, fmt.Errorf("repository %q must be owner/name", entry)
		}
		if err := validateRepositoryName("owner", entryOwner); err != nil {
			return nil, nil, err
		}
		if err := validateRepositoryName("repository", entryName); err != nil {
			return nil, nil, err
		}
		if owner == "" {
			owner = entryOwner
		}
		if !strings.EqualFold(owner, entryOwner) {
			return nil, nil, fmt.Errorf("repositories %q and %q name two owners, one installation belongs to one account", full[0], entry)
		}
		// GitHub treats a repository name as case insensitive, so two spellings
		// of one repository are one repository. Listing both is a mistake worth
		// naming rather than a request for two.
		key := strings.ToLower(entry)
		if seen[key] {
			return nil, nil, fmt.Errorf("repository %q is listed twice", entry)
		}
		seen[key] = true
		full = append(full, entry)
		names = append(names, entryName)
	}
	return full, names, nil
}

// validateRepositoryName holds a name to the characters GitHub allows.
func validateRepositoryName(field, value string) error {
	if value == "" {
		return fmt.Errorf("a %s name is required", field)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%s name %q must not traverse", field, value)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s name %q may only contain letters, digits, and -._", field, value)
		}
	}
	return nil
}

// checkedPermissions copies and validates a required grant.
func checkedPermissions(permissions map[string]string) (map[string]string, error) {
	if len(permissions) == 0 {
		return nil, nil
	}
	checked := make(map[string]string, len(permissions))
	for name, level := range permissions {
		if name == "" {
			return nil, errors.New("a permission name is required")
		}
		rank, known := permissionLevels[level]
		if !known {
			return nil, fmt.Errorf("permission %q level %q must be read, write, or admin", name, level)
		}
		if rank == 0 {
			return nil, fmt.Errorf("permission %q must ask for read, write, or admin, not none", name)
		}
		checked[name] = level
	}
	return checked, nil
}
