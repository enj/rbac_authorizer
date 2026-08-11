// Package gitcli is the only Git subprocess and credential boundary in the
// engine.
//
// Every command is built as an explicit argument vector and executed with
// exec.CommandContext. No command is ever composed through sh, bash, or -c, so
// there is no string that a ref name, path, or upstream commit message could
// escape into. Credentials never appear in an argument vector or a remote URL.
// They are supplied through the controlled process environment and seeded into
// a redactor before the first subprocess starts.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultBinary is the Git executable looked up on PATH when none is given.
const DefaultBinary = "git"

// PublishHost is the only host the engine may push to over the network.
const PublishHost = "github.com"

// redactedUser replaces user information in a rendered remote URL.
const redactedUser = "redacted"

// defaultInherit lists the process environment variables a Git subprocess may
// see. Everything else is dropped so runs do not depend on ambient state.
var defaultInherit = []string{"PATH", "HOME", "SYSTEMROOT"}

// fixedEnv is applied to every Git subprocess. Global and system configuration
// are neutralised so ambient developer or runner settings can never change what
// the engine produces, terminal prompts are disabled so a missing credential
// fails instead of hanging, and the C locale keeps parsed output stable.
var fixedEnv = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_SYSTEM=" + os.DevNull,
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"LC_ALL=C",
	"LANG=C",
}

// fixedConfig is prepended to every Git command. Repository local hooks are
// disabled because the engine materialises untrusted upstream content and a
// checked out hook must never execute.
var fixedConfig = []string{"-c", "core.hooksPath=" + os.DevNull}

// commandWaitDelay bounds how long a killed subprocess and its descendants may
// keep the output pipes open. Without it a grandchild holding a pipe would
// block cancellation forever.
const commandWaitDelay = 5 * time.Second

// Options configures a Runner.
type Options struct {
	// Binary is the Git executable. Empty means look up DefaultBinary on PATH.
	Binary string
	// Dir is the working directory for every command. Empty means the process
	// working directory.
	Dir string
	// Inherit names the process environment variables to pass through. Empty
	// means the default set.
	Inherit []string
	// Env holds additional KEY=VALUE entries applied after the inherited and
	// fixed entries, which is how credentials reach Git. Every non-empty value
	// is seeded into the redactor, so an entry cannot leak by being forgotten in
	// Secrets.
	Env []string
	// Secrets holds additional exact values that must never appear in captured
	// output.
	Secrets []string
}

// Runner executes Git commands with a controlled environment.
type Runner struct {
	binary   string
	dir      string
	env      []string
	redactor *Redactor
}

// New resolves the Git executable and builds the subprocess environment.
func New(ctx context.Context, opts Options) (*Runner, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("git runner: %w", err)
	}
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	if !strings.ContainsRune(binary, filepath.Separator) && !strings.ContainsRune(binary, '/') {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			return nil, fmt.Errorf("git executable lookup: %w", err)
		}
		binary = resolved
	}
	if opts.Dir != "" {
		info, err := os.Stat(opts.Dir)
		if err != nil {
			return nil, fmt.Errorf("git working directory: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("git working directory: %q is not a directory", opts.Dir)
		}
	}
	return &Runner{
		binary:   binary,
		dir:      opts.Dir,
		env:      buildEnv(opts.Inherit, opts.Env),
		redactor: NewRedactor(append(envValues(opts.Env), opts.Secrets...)...),
	}, nil
}

// envValues reports the value half of every KEY=VALUE entry. Redaction is
// seeded from them so a credential passed through Env is covered even when the
// caller forgets to list it in Secrets.
func envValues(entries []string) []string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, value, ok := strings.Cut(entry, "="); ok && value != "" {
			values = append(values, value)
		}
	}
	return values
}

// Binary reports the resolved Git executable path.
func (r *Runner) Binary() string { return r.binary }

// Dir reports the working directory used for every command.
func (r *Runner) Dir() string { return r.dir }

// Redactor reports the redactor seeded with this runner's secrets.
func (r *Runner) Redactor() *Redactor { return r.redactor }

// WithDir returns a copy of the runner that operates in dir.
func (r *Runner) WithDir(dir string) *Runner {
	clone := *r
	clone.dir = dir
	return &clone
}

// buildEnv assembles the subprocess environment from inherited names, the fixed
// entries, and caller supplied entries. Later entries win, which lets a caller
// override HOME for an isolated run.
func buildEnv(inherit, extra []string) []string {
	names := inherit
	if len(names) == 0 {
		names = defaultInherit
	}
	env := make([]string, 0, len(names)+len(fixedEnv)+len(extra))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, fixedEnv...)
	return append(env, extra...)
}

// ExecError reports a Git command that exited non-zero or could not run. Every
// field is redacted.
type ExecError struct {
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

// Error renders the failing command, its exit status, and the first line of its
// diagnostic output.
func (e *ExecError) Error() string {
	message := fmt.Sprintf("git %s", strings.Join(e.Args, " "))
	if e.ExitCode > 0 {
		message += ": exit status " + strconv.Itoa(e.ExitCode)
	} else if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	if line := firstLine(e.Stderr); line != "" {
		message += ": " + line
	}
	return message
}

// Unwrap exposes the underlying execution error.
func (e *ExecError) Unwrap() error { return e.Err }

// ExitCodeOf reports the process exit code carried by err, or zero.
func ExitCodeOf(err error) int {
	var execErr *ExecError
	if errors.As(err, &execErr) {
		return execErr.ExitCode
	}
	return 0
}

// run executes one Git command and returns its redacted standard output.
func (r *Runner) run(ctx context.Context, args ...string) (string, error) {
	return r.runWith(ctx, nil, args...)
}

// runWith executes one Git command with additional environment entries. The
// entries are appended last so they win over the runner environment, which is
// how per commit identity is supplied without touching an argument vector.
func (r *Runner) runWith(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	outWriter := r.redactor.Writer(&stdout)
	errWriter := r.redactor.Writer(&stderr)

	full := append(append([]string{}, fixedConfig...), args...)
	cmd := exec.CommandContext(ctx, r.binary, full...)
	cmd.Dir = r.dir
	cmd.Env = r.env
	if len(extraEnv) > 0 {
		cmd.Env = append(append([]string{}, r.env...), extraEnv...)
	}
	cmd.Stdout = outWriter
	cmd.Stderr = errWriter
	cmd.Stdin = nil
	cmd.WaitDelay = commandWaitDelay

	runErr := cmd.Run()
	closeErr := errors.Join(outWriter.Close(), errWriter.Close())
	if closeErr != nil {
		closeErr = fmt.Errorf("git output capture: %w", closeErr)
	}
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), errors.Join(fmt.Errorf("git %s: %w", strings.Join(r.redactor.Strings(args), " "), ctxErr), closeErr)
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return stdout.String(), errors.Join(&ExecError{
			Args:     r.redactor.Strings(args),
			ExitCode: exitCode,
			Stderr:   stderr.String(),
			Err:      runErr,
		}, closeErr)
	}
	if closeErr != nil {
		return stdout.String(), closeErr
	}
	return stdout.String(), nil
}

// Version is a parsed Git or Go release version.
type Version struct {
	Major int
	Minor int
	Patch int
}

// String renders the version as major.minor.patch.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AtLeast reports whether v is not older than other.
func (v Version) AtLeast(other Version) bool {
	switch {
	case v.Major != other.Major:
		return v.Major > other.Major
	case v.Minor != other.Minor:
		return v.Minor > other.Minor
	default:
		return v.Patch >= other.Patch
	}
}

// ParseVersion parses a dotted numeric version, ignoring any vendor suffix.
func ParseVersion(value string) (Version, error) {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == '.' })
	if len(fields) == 0 {
		return Version{}, fmt.Errorf("version %q has no numeric components", value)
	}
	var version Version
	targets := []*int{&version.Major, &version.Minor, &version.Patch}
	for i, target := range targets {
		if i >= len(fields) {
			break
		}
		digits := leadingDigits(fields[i])
		if digits == "" {
			if i == 0 {
				return Version{}, fmt.Errorf("version %q has no numeric components", value)
			}
			break
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", value, err)
		}
		*target = n
	}
	return version, nil
}

// Version reports the version of the resolved Git executable.
func (r *Runner) Version(ctx context.Context) (Version, error) {
	out, err := r.run(ctx, "--version")
	if err != nil {
		return Version{}, fmt.Errorf("git version: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return Version{}, fmt.Errorf("git version: unexpected output %q", firstLine(out))
	}
	version, err := ParseVersion(fields[2])
	if err != nil {
		return Version{}, fmt.Errorf("git version: %w", err)
	}
	return version, nil
}

// InitRepository creates a repository in the runner's working directory.
func (r *Runner) InitRepository(ctx context.Context, initialBranch string) error {
	if err := ValidateBranchName(initialBranch); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	if _, err := r.run(ctx, "init", "--quiet", "--initial-branch="+initialBranch); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	return nil
}

// notARepository is the exact prefix git uses when discovery simply found no
// repository. Every other fatal error, such as dubious ownership or a broken
// gitdir pointer, also exits 128 and must be reported rather than swallowed.
const notARepository = "not a git repository (or any"

// IsRepository reports whether the working directory is inside a Git work tree.
// A directory that is not a repository yet is a supported state; a repository
// that cannot be read is an error.
func (r *Runner) IsRepository(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		var execErr *ExecError
		if errors.As(err, &execErr) && strings.Contains(execErr.Stderr, notARepository) {
			return false, nil
		}
		return false, fmt.Errorf("git work tree probe: %w", err)
	}
	return strings.TrimSpace(out) == "true", nil
}

// RepositoryRoot reports the absolute path of the work tree root.
func (r *Runner) RepositoryRoot(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git repository root: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// HasHead reports whether HEAD resolves to an existing object. A freshly
// initialized repository has no HEAD commit and is not an error, while a HEAD
// that points at a missing or unreadable object is.
//
// show-ref is used rather than rev-parse because rev-parse --quiet reports both
// states with exit status 1, which would hide a corrupt repository.
func (r *Runner) HasHead(ctx context.Context) (bool, error) {
	_, err := r.run(ctx, "show-ref", "--head", "--verify", "--quiet", "--", "HEAD")
	if err == nil {
		return true, nil
	}
	if ExitCodeOf(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git head probe: %w", err)
}

// ResolveCommit resolves a revision to a full commit object name.
func (r *Runner) ResolveCommit(ctx context.Context, revision string) (string, error) {
	if err := validateArgument("revision", revision); err != nil {
		return "", fmt.Errorf("git revision: %w", err)
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("git revision %q: %w", r.redactor.String(revision), err)
	}
	return strings.TrimSpace(out), nil
}

// ConfigLocal reads one repository local configuration value. A missing key is
// reported through found rather than as an error.
func (r *Runner) ConfigLocal(ctx context.Context, key string) (value string, found bool, err error) {
	if err := validateArgument("config key", key); err != nil {
		return "", false, fmt.Errorf("git config: %w", err)
	}
	out, err := r.run(ctx, "config", "--local", "--get", "--end-of-options", key)
	if err != nil {
		if ExitCodeOf(err) == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return strings.TrimRight(out, "\n"), true, nil
}

// SetConfigLocal writes one repository local configuration value. Values that
// look like options are rejected because git config has no option terminator.
func (r *Runner) SetConfigLocal(ctx context.Context, key, value string) error {
	if err := validateArgument("config key", key); err != nil {
		return fmt.Errorf("git config: %w", err)
	}
	if err := validateArgument("config value", value); err != nil {
		return fmt.Errorf("git config %s: %w", r.redactor.String(key), r.redactor.Error(err))
	}
	if _, err := r.run(ctx, "config", "--local", "--replace-all", key, value); err != nil {
		return fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return nil
}

// UnsetConfigLocal removes one repository local configuration key. Removing a
// key that is already absent is not an error.
func (r *Runner) UnsetConfigLocal(ctx context.Context, key string) error {
	if err := validateArgument("config key", key); err != nil {
		return fmt.Errorf("git config: %w", err)
	}
	_, err := r.run(ctx, "config", "--local", "--unset-all", "--end-of-options", key)
	if err != nil && ExitCodeOf(err) != 5 {
		return fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return nil
}

// Trailer is one parsed commit message trailer.
type Trailer struct {
	Key   string
	Value string
}

// Commit is the metadata the engine needs about one commit.
type Commit struct {
	SHA             string
	AuthorName      string
	AuthorEmail     string
	AuthorDate      string
	CommitterName   string
	CommitterEmail  string
	CommitterDate   string
	SignatureStatus string
	SignerKey       string
	Signer          string
	Subject         string
	// RawMessage is the complete commit message including the subject line, so
	// replaying it must not append the subject again.
	RawMessage string
	Trailers   []Trailer
}

// Identity renders a name and email in Git identity form.
func Identity(name, email string) string {
	return fmt.Sprintf("%s <%s>", name, email)
}

// AuthorIdentity renders the commit author identity.
func (c Commit) AuthorIdentity() string { return Identity(c.AuthorName, c.AuthorEmail) }

// CommitterIdentity renders the commit committer identity.
func (c Commit) CommitterIdentity() string { return Identity(c.CommitterName, c.CommitterEmail) }

// TrailerValues reports every value recorded under key.
func (c Commit) TrailerValues(key string) []string {
	var values []string
	for _, trailer := range c.Trailers {
		if strings.EqualFold(trailer.Key, key) {
			values = append(values, trailer.Value)
		}
	}
	return values
}

// commitFormat requests every commit field in one pass, separated by null
// bytes so that multi line values cannot be confused with field boundaries.
const commitFormat = "%H%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%G?%x00%GK%x00%GS%x00%s%x00%B%x00%(trailers:only=true,unfold=true)"

// commitFieldCount is the number of fields commitFormat produces.
const commitFieldCount = 13

// CommitInfo reads metadata, signature status, and trailers for one revision.
func (r *Runner) CommitInfo(ctx context.Context, revision string) (Commit, error) {
	if err := validateArgument("revision", revision); err != nil {
		return Commit{}, fmt.Errorf("git commit metadata: %w", err)
	}
	out, err := r.run(ctx, "log", "-1", "--no-patch", "--format="+commitFormat, "--end-of-options", revision, "--")
	if err != nil {
		return Commit{}, fmt.Errorf("git commit metadata for %q: %w", r.redactor.String(revision), err)
	}
	fields := strings.SplitN(out, "\x00", commitFieldCount)
	if len(fields) != commitFieldCount {
		return Commit{}, fmt.Errorf("git commit metadata for %q: got %d fields, want %d", r.redactor.String(revision), len(fields), commitFieldCount)
	}
	commit := Commit{
		SHA:             fields[0],
		AuthorName:      fields[1],
		AuthorEmail:     fields[2],
		AuthorDate:      fields[3],
		CommitterName:   fields[4],
		CommitterEmail:  fields[5],
		CommitterDate:   fields[6],
		SignatureStatus: fields[7],
		SignerKey:       fields[8],
		Signer:          fields[9],
		Subject:         fields[10],
		RawMessage:      fields[11],
		Trailers:        parseTrailers(fields[12]),
	}
	return commit, nil
}

// parseTrailers splits unfolded trailer lines into key and value pairs.
func parseTrailers(block string) []Trailer {
	var trailers []Trailer
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		trailers = append(trailers, Trailer{Key: strings.TrimSpace(key), Value: strings.TrimSpace(value)})
	}
	return trailers
}

// Signature is a Git author or committer identity with an optional date. The
// date uses any format git accepts, such as RFC 3339.
type Signature struct {
	Name  string
	Email string
	Date  string
}

// env renders the signature as environment entries for the given role.
func (s Signature) env(role string) []string {
	var entries []string
	if s.Name != "" {
		entries = append(entries, "GIT_"+role+"_NAME="+s.Name)
	}
	if s.Email != "" {
		entries = append(entries, "GIT_"+role+"_EMAIL="+s.Email)
	}
	if s.Date != "" {
		entries = append(entries, "GIT_"+role+"_DATE="+s.Date)
	}
	return entries
}

// AddPaths stages exact repository relative paths. Pathspec magic and wildcards
// are rejected, and literal pathspecs are forced for the duration of the
// command, so a configured path can only ever stage the file it names.
func (r *Runner) AddPaths(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return fmt.Errorf("git add: at least one path is required")
	}
	for _, path := range paths {
		if err := validateArgument("path", path); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
		if err := validateLiteralPath(path); err != nil {
			return fmt.Errorf("git add: %w", err)
		}
	}
	args := append([]string{"add", "--"}, paths...)
	if _, err := r.runWith(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, args...); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	return nil
}

// validateLiteralPath rejects pathspec magic and glob characters.
func validateLiteralPath(path string) error {
	if strings.HasPrefix(path, ":") {
		return fmt.Errorf("path %q must not use pathspec magic", path)
	}
	if strings.ContainsAny(path, "*?[") {
		return fmt.Errorf("path %q must name one file, not a pattern", path)
	}
	return nil
}

// CommitOptions describes one commit created through the Git command line.
type CommitOptions struct {
	Message    string
	Author     Signature
	Committer  Signature
	AllowEmpty bool
	// Sign requests a signed commit. Generated replay commits are never signed,
	// so this stays false everywhere except fixtures and bootstrap commits that
	// must carry a real signature.
	Sign bool
}

// Commit records the staged tree. Identity travels through the environment, the
// message is preserved verbatim, and signing is always off, because the engine
// only creates generated or bootstrap commits.
func (r *Runner) Commit(ctx context.Context, opts CommitOptions) error {
	if opts.Message == "" {
		return fmt.Errorf("git commit: a message is required")
	}
	args := []string{"commit", "--quiet", "--no-verify", "--cleanup=verbatim"}
	if opts.Sign {
		args = append(args, "--gpg-sign")
	} else {
		args = append(args, "--no-gpg-sign")
	}
	if opts.AllowEmpty {
		args = append(args, "--allow-empty")
	}
	args = append(args, "-m", opts.Message)
	env := append(opts.Author.env("AUTHOR"), opts.Committer.env("COMMITTER")...)
	if _, err := r.runWith(ctx, env, args...); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	return nil
}

// Push updates remote refs. There is no force variant: every refspec is checked
// and any forced or deleting refspec is rejected before the subprocess starts.
func (r *Runner) Push(ctx context.Context, remote string, refspecs ...string) error {
	if err := ValidatePushRemote(remote); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	if len(refspecs) == 0 {
		return fmt.Errorf("git push: at least one refspec is required")
	}
	for _, spec := range refspecs {
		if err := ValidatePushRefspec(spec); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
	}
	if err := r.assertNoRemoteRewrites(ctx, remote); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	// The empty credential.helper resets the helper list, so a repository local
	// helper can neither be consulted nor prompt. Hooks are already disabled for
	// every command and --no-verify keeps the pre-push hook off as well.
	args := []string{"-c", "credential.helper=", "push", "--atomic", "--porcelain", "--no-verify", "--", remote}
	out, err := r.run(ctx, append(args, refspecs...)...)
	if err != nil {
		// Porcelain output carries the per ref verdict on standard output, so
		// the rejection reason would otherwise be lost.
		if verdict := rejectedRef(out); verdict != "" {
			return fmt.Errorf("git push to %q: %s: %w", redactRemote(remote), verdict, err)
		}
		return fmt.Errorf("git push to %q: %w", redactRemote(remote), err)
	}
	return nil
}

// assertNoRemoteRewrites fails closed when repository local configuration could
// silently redirect a network push. Global and system configuration are already
// neutralised for every subprocess, so only local rewrites remain.
func (r *Runner) assertNoRemoteRewrites(ctx context.Context, remote string) error {
	if !isNetworkRemote(remote) {
		return nil
	}
	for _, pattern := range []string{`^url\..*\.(insteadof|pushinsteadof)$`, `^remote\..*\.pushurl$`} {
		out, err := r.run(ctx, "config", "--local", "--name-only", "--get-regexp", "--end-of-options", pattern)
		switch {
		case err == nil:
			names := strings.Fields(out)
			return fmt.Errorf("repository local %s rewrites the push target", strings.Join(names, ", "))
		case ExitCodeOf(err) == 1:
		default:
			return fmt.Errorf("read local push configuration: %w", err)
		}
	}
	return nil
}

// isNetworkRemote reports whether a remote reaches the network.
func isNetworkRemote(remote string) bool {
	return strings.HasPrefix(remote, "https://")
}

// rejectedRef returns the first rejected ref line of git push porcelain output.
func rejectedRef(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "!") {
			return strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(line, "!"), "\t", " "))
		}
	}
	return ""
}

// redactRemote renders a remote without any user information so an error can
// never echo a credential that was passed by mistake. Both URL and scp like
// forms are covered, and an unparseable value is replaced entirely.
func redactRemote(remote string) string {
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return Placeholder
		}
		if parsed.User == nil {
			return remote
		}
		// The placeholder carries no characters that URL encoding would escape.
		parsed.User = url.User(redactedUser)
		return parsed.String()
	}
	if at := strings.LastIndex(remote, "@"); at >= 0 {
		return redactedUser + "@" + remote[at+1:]
	}
	return remote
}

// ValidateRemote checks a fetch or local push target. Credentials may never
// travel in a remote URL, so any embedded user information is rejected.
func ValidateRemote(remote string) error {
	if err := validateRemoteArgument(remote); err != nil {
		return err
	}
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return fmt.Errorf("remote %q is malformed: %s", redactRemote(remote), urlErrorReason(err))
		}
		switch parsed.Scheme {
		case "https", "file":
		default:
			return fmt.Errorf("remote %q must use https or file", redactRemote(remote))
		}
		if parsed.User != nil {
			return fmt.Errorf("remote %q must not embed credentials", redactRemote(remote))
		}
		return nil
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	if strings.ContainsAny(remote, ":@") {
		return fmt.Errorf("remote %q must be a remote name, an absolute path, or an https URL", redactRemote(remote))
	}
	return nil
}

// ValidatePushRemote checks a push target. A push may carry credentials, so the
// target must be explicit: named remotes are rejected because their URL lives in
// configuration, and an https target must be the one host the engine publishes
// to. Absolute paths and file URLs remain available for local verification.
func ValidatePushRemote(remote string) error {
	if err := ValidateRemote(remote); err != nil {
		return err
	}
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return fmt.Errorf("remote %q is malformed: %s", redactRemote(remote), urlErrorReason(err))
		}
		if parsed.Scheme == "https" && parsed.Hostname() != PublishHost {
			return fmt.Errorf("remote %q must publish to %s", redactRemote(remote), PublishHost)
		}
		return nil
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	return fmt.Errorf("remote %q must be an absolute path or an https URL, a named remote hides its target in configuration", redactRemote(remote))
}

// urlErrorReason reports why a URL failed to parse without echoing the URL,
// which url.Error embeds in its own message.
func urlErrorReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return "unparseable"
}

// validateRemoteArgument rejects empty, option like, and null bearing remotes
// without echoing a credential.
func validateRemoteArgument(remote string) error {
	switch {
	case remote == "":
		return errors.New("remote must not be empty")
	case strings.HasPrefix(remote, "-"):
		return fmt.Errorf("remote %q: %w", redactRemote(remote), ErrFlagLikeArgument)
	case strings.ContainsRune(remote, '\x00'):
		return fmt.Errorf("remote %q must not contain a null byte", redactRemote(remote))
	}
	return nil
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// leadingDigits returns the leading decimal digits of s.
func leadingDigits(s string) string {
	for i, r := range s {
		if r < '0' || r > '9' {
			return s[:i]
		}
	}
	return s
}
