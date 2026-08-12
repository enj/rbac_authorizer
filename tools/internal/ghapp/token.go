package ghapp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/ghapi"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Installation describes what a minted installation token is allowed to do.
//
// It carries the grant and never the credential. A caller reads it to decide
// whether a run can proceed, and to record in a report what the run was
// permitted to do, which is exactly the part of a token that is safe to write
// down.
type Installation struct {
	// ID is the installation the token was minted for.
	ID int64

	// ExpiresAt is when GitHub stops honouring the token, in UTC.
	ExpiresAt time.Time

	// RepositorySelection is all or selected, as GitHub reported it.
	RepositorySelection string

	// Permissions maps a permission name to the level granted.
	Permissions map[string]string

	// Repositories are the owner/name repositories the token reaches, sorted.
	// It is empty when RepositorySelection is all.
	Repositories []string
}

// clone copies an Installation so a caller cannot reach the cached grant.
func (i Installation) clone() Installation {
	i.Permissions = maps.Clone(i.Permissions)
	i.Repositories = slices.Clone(i.Repositories)
	return i
}

// AuthorizationHeader reports the Authorization header value for one REST
// request, minting or renewing the installation token as needed. It satisfies
// ghapi.Authorizer, so a REST client renews automatically per request.
func (a *App) AuthorizationHeader(ctx context.Context) (string, error) {
	token, _, err := a.credential(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}

// WithCredential calls use with the bare installation token.
//
// This is the accessor for the Git boundary, which must place the value in a
// subprocess environment and so cannot take a header. The value is passed
// rather than returned so its use has a visible end, and an error from use is
// redacted on the way out, because a caller that was handed a token is the
// caller most likely to put it in a message.
func (a *App) WithCredential(ctx context.Context, use func(token string) error) error {
	if use == nil {
		return errors.New("github installation token: no credential callback")
	}
	token, _, err := a.credential(ctx)
	if err != nil {
		return err
	}
	if err := use(token); err != nil {
		return a.redact(err)
	}
	return nil
}

// Installation reports the current grant, minting a token if there is none.
func (a *App) Installation(ctx context.Context) (Installation, error) {
	_, installation, err := a.credential(ctx)
	if err != nil {
		return Installation{}, err
	}
	return installation.clone(), nil
}

// Redactor reports a redactor seeded with every token this App has minted.
//
// It is a snapshot. A token minted after this call is not in the returned
// redactor, so a caller that holds one across a renewal must ask again; the
// Git boundary does, because it builds a runner per credentialed operation.
func (a *App) Redactor() *gitcli.Redactor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.redactor
}

// jwtHeader is the authorizer of the client this App mints through. The App JWT
// authenticates as the App itself, which is the only credential that does not
// depend on the installation token being obtained.
func (a *App) jwtHeader(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("mint app jwt: %w", err)
	}
	token, err := appJWT(a.key, a.appID, a.clock())
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}

// credential reports a live installation token and its grant.
//
// Concurrent callers mint once. The first to pass the gate performs the request
// and caches the result; the ones behind it find that result when they arrive
// and never reach GitHub. The gate is a channel rather than a mutex so a caller
// waiting behind a slow mint still honours its own cancellation, and the wait
// is the only thing cancellation interrupts: a mint already in flight belongs
// to the context that started it.
//
// A grant that failed verification is remembered and returned to every later
// caller without minting again. That refusal is a pure function of this App's
// configuration and the installation's grant on GitHub, neither of which a
// retry changes, so minting again would burn a live token per request against
// the installation's budget to reach the same answer.
func (a *App) credential(ctx context.Context) (string, Installation, error) {
	if err := ctx.Err(); err != nil {
		return "", Installation{}, fmt.Errorf("github installation token: %w", err)
	}
	if failure := a.terminal(); failure != nil {
		return "", Installation{}, failure
	}
	if token, installation, ok := a.cached(); ok {
		return token, installation, nil
	}
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return "", Installation{}, fmt.Errorf("github installation token: %w", ctx.Err())
	}
	defer func() { <-a.gate }()
	if failure := a.terminal(); failure != nil {
		return "", Installation{}, failure
	}
	if token, installation, ok := a.cached(); ok {
		return token, installation, nil
	}
	return a.mint(ctx)
}

// terminal reports the recorded verification refusal, if there is one.
//
// It outranks the cached token and outlives its expiry: nothing this App can do
// makes a grant it already rejected acceptable.
func (a *App) terminal() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verifyFailure
}

// cached reports the held token when it is still usable.
func (a *App) cached() (string, Installation, bool) {
	now := a.clock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == "" {
		return "", Installation{}, false
	}
	// The margin is subtracted from expiry rather than added to now so a token
	// is replaced before an operation started with it could outlive it.
	if !now.Before(a.current.ExpiresAt.Add(-a.renewBefore)) {
		return "", Installation{}, false
	}
	return a.token, a.current, true
}

// mint requests one installation token and verifies the grant it carries.
func (a *App) mint(ctx context.Context) (string, Installation, error) {
	minted, err := a.client.CreateInstallationToken(ctx, a.installationID, ghapi.InstallationTokenRequest{
		Repositories: a.names,
		Permissions:  a.want.Permissions,
	})
	if err != nil {
		return "", Installation{}, a.redact(fmt.Errorf("mint installation token: %w", err))
	}
	// Everything below this line describes a token that now exists, so the
	// redactor is seeded before any of it can raise an error.
	a.seed(minted.Token)
	installation, err := a.verify(minted, a.clock())
	if err != nil {
		// The refusal is recorded, not just returned. It cannot come out
		// differently on a retry, and a caller that keeps asking would mint a
		// live token per request to be told the same thing.
		failure := a.redact(fmt.Errorf("mint installation token: %w", err))
		a.storeFailure(failure)
		return "", Installation{}, failure
	}
	a.store(minted.Token, installation)
	return minted.Token, installation, nil
}

// verify checks a minted token against what this App asked for.
func (a *App) verify(minted ghapi.InstallationToken, now time.Time) (Installation, error) {
	if strings.TrimSpace(minted.Token) == "" {
		return Installation{}, errors.New("github returned an empty token")
	}
	if minted.ExpiresAt.IsZero() {
		return Installation{}, errors.New("github returned a token with no expiry")
	}
	// A token that is already inside the renewal margin would be replaced by
	// the next call that asked for it, so accepting one would turn every
	// request into a mint rather than produce a usable credential.
	if !minted.ExpiresAt.After(now.Add(a.renewBefore)) {
		return Installation{}, fmt.Errorf("github returned a token expiring at %s, inside the %s renewal margin",
			minted.ExpiresAt.UTC().Format(time.RFC3339), a.renewBefore)
	}

	granted := Installation{
		ID:                  a.installationID,
		ExpiresAt:           minted.ExpiresAt.UTC(),
		RepositorySelection: minted.RepositorySelection,
		Permissions:         maps.Clone(minted.Permissions),
	}
	for _, repository := range minted.Repositories {
		granted.Repositories = append(granted.Repositories, repository.FullName)
	}
	slices.Sort(granted.Repositories)

	if err := a.verifyPermissions(granted.Permissions); err != nil {
		return Installation{}, err
	}
	if err := a.verifyRepositories(granted); err != nil {
		return Installation{}, err
	}
	return granted, nil
}

// verifyPermissions checks the granted levels against the required ones.
func (a *App) verifyPermissions(granted map[string]string) error {
	for name, required := range a.want.Permissions {
		level, ok := granted[name]
		if !ok {
			return fmt.Errorf("the token grants no %q permission, %q is required: %w", name, required, ErrInsufficientPermission)
		}
		if permissionLevels[level] < permissionLevels[required] {
			return fmt.Errorf("the token grants %q on %q, %q is required: %w", level, name, required, ErrInsufficientPermission)
		}
	}
	return nil
}

// verifyRepositories checks that the token reaches every required repository
// and no repository that was not required.
//
// The check runs in both directions. A token missing a destination fails the
// run later, at the push, which is the expensive place to find out. A token
// carrying a repository nobody asked for is the more serious of the two: the
// App is installed only where it must write, and a credential reaching further
// than the request is the thing that arrangement exists to prevent, so
// receiving one means an assumption this run is built on is already wrong.
//
// Names are compared case insensitively, because GitHub treats owner and
// repository names that way and a token would otherwise pass or fail on
// spelling alone.
func (a *App) verifyRepositories(granted Installation) error {
	if len(a.want.Repositories) == 0 {
		return nil
	}
	if !strings.EqualFold(granted.RepositorySelection, "selected") {
		return fmt.Errorf("the token covers %q repositories although %d were requested: %w",
			granted.RepositorySelection, len(a.want.Repositories), ErrRepositoryScope)
	}
	requested := make(map[string]bool, len(a.want.Repositories))
	for _, wanted := range a.want.Repositories {
		requested[strings.ToLower(wanted)] = true
	}
	reachable := make(map[string]bool, len(granted.Repositories))
	for _, repository := range granted.Repositories {
		key := strings.ToLower(repository)
		reachable[key] = true
		if !requested[key] {
			return fmt.Errorf("the token also reaches %q, which was not requested: %w", repository, ErrRepositoryScope)
		}
	}
	for _, wanted := range a.want.Repositories {
		if !reachable[strings.ToLower(wanted)] {
			return fmt.Errorf("the token does not reach %q: %w", wanted, ErrRepositoryScope)
		}
	}
	return nil
}

// seed adds an exact secret value to the redactor.
func (a *App) seed(secret string) {
	if secret == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if slices.Contains(a.secrets, secret) {
		return
	}
	// Every token this App has ever minted stays seeded. A renewal does not
	// make the previous value safe to print, and a report is written after the
	// run rather than during it.
	a.secrets = append(a.secrets, secret)
	a.redactor = gitcli.NewRedactor(a.secrets...)
}

// store caches a verified credential.
func (a *App) store(token string, installation Installation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = token
	a.current = installation
}

// storeFailure records a terminal verification refusal.
func (a *App) storeFailure(failure error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verifyFailure = failure
	// The token that failed verification is dropped rather than held. It may
	// well be a working credential, and the point of the refusal is that this
	// run must not use it.
	a.token = ""
	a.current = Installation{}
}

// redact removes every minted token from an error.
func (a *App) redact(err error) error {
	a.mu.Lock()
	redactor := a.redactor
	a.mu.Unlock()
	return redactor.Error(err)
}
