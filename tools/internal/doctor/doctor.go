// Package doctor reports whether the local environment satisfies the Soapbox
// toolchain, identity, and commit signing policy.
//
// Every check is reported, and the report fails only when a required check
// fails, so one run gives a complete picture of what needs fixing. A directory
// that is not a repository yet, or a repository without a HEAD commit, is a
// supported state rather than an error.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// commandWaitDelay bounds how long a killed probe and its descendants may keep
// the output pipes open.
const commandWaitDelay = 5 * time.Second

// Status is the outcome of one check.
type Status string

// Check outcomes.
const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusWarn Status = "WARN"
	StatusSkip Status = "SKIP"
)

// Check is one environment diagnostic.
type Check struct {
	Name     string
	Status   Status
	Required bool
	Detail   string
}

// Policy is the identity and signing contract every Soapbox commit satisfies.
type Policy struct {
	AuthorName          string
	AuthorEmail         string
	SigningKeyPath      string
	SignoffTrailerKey   string
	SignoffTrailerValue string
	// MinimumGit is the oldest usable Git release. It is gitcli.MinimumVersion
	// rather than a number of its own, because a doctor that reported a floor
	// the engine does not actually enforce would pass a machine every later
	// command then refuses.
	MinimumGit gitcli.Version
	MinimumGo  gitcli.Version
	Toolchain  string
}

// SoapboxPolicy is the policy for the enj/soapbox repository itself. Every
// commit is SSH signed, authored and committed by Monis Khan <i@monis.app>, and
// carries exactly one Signed-off-by trailer for mok@microsoft.com. The signoff
// email deliberately differs from the author email, which is why git commit -s
// must never be used.
func SoapboxPolicy() Policy {
	return Policy{
		AuthorName:          "Monis Khan",
		AuthorEmail:         "i@monis.app",
		SigningKeyPath:      "/Users/mo/.config/wunderkind/ssh/id_ed25519.pub",
		SignoffTrailerKey:   "Signed-off-by",
		SignoffTrailerValue: "Monis Khan <mok@microsoft.com>",
		MinimumGit:          gitcli.MinimumVersion(),
		MinimumGo:           gitcli.Version{Major: 1, Minor: 26},
		Toolchain:           buildinfo.Toolchain,
	}
}

// Identity renders the policy author identity.
func (p Policy) Identity() string { return gitcli.Identity(p.AuthorName, p.AuthorEmail) }

// SignoffTrailer renders the required trailer line.
func (p Policy) SignoffTrailer() string {
	return p.SignoffTrailerKey + ": " + p.SignoffTrailerValue
}

// Options configures one doctor run.
type Options struct {
	// Dir is the repository directory to inspect. Empty means the process
	// working directory.
	Dir string
	// Git is the runner used for repository checks. A nil runner is created for
	// Dir, and a failure to create it is reported as a failed check.
	Git *gitcli.Runner
	// GoBinary is the Go executable to probe. Empty means go on PATH.
	GoBinary string
	// Policy overrides the Soapbox policy, which is what tests use to describe
	// the identity of a temporary repository.
	Policy *Policy
}

// Run executes every check and returns the complete report.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("doctor run: %w", err)
	}
	policy := SoapboxPolicy()
	if opts.Policy != nil {
		policy = *opts.Policy
	}
	dir := opts.Dir
	if dir == "" {
		working, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("doctor working directory: %w", err)
		}
		dir = working
	}

	b := &builder{}
	git := checkGit(ctx, b, opts, dir, policy)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("doctor run: %w", err)
	}
	checkGo(ctx, b, opts, policy)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("doctor run: %w", err)
	}
	if git == nil {
		b.skipRemaining(repositoryCheckNames, "git is unavailable")
		return b.report(), nil
	}
	checkRepository(ctx, b, git, policy)
	// A run that was cancelled midway reports a cancellation rather than a
	// policy verdict, because the checks that did not run prove nothing.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("doctor run: %w", err)
	}
	return b.report(), nil
}

// goCaches names the Go environment variables the engine must be able to write,
// paired with the check each one reports under.
var goCaches = []struct {
	variable string
	check    string
}{
	{variable: "GOPATH", check: "go.cache.gopath"},
	{variable: "GOMODCACHE", check: "go.cache.gomodcache"},
	{variable: "GOCACHE", check: "go.cache.gocache"},
}

// goCacheChecks reports the check names of the cache probes.
func goCacheChecks() []string {
	names := make([]string, 0, len(goCaches))
	for _, cache := range goCaches {
		names = append(names, cache.check)
	}
	return names
}

// repositoryCheckNames are the checks that need a working Git executable.
var repositoryCheckNames = []string{
	"repo.present",
	"repo.user.name",
	"repo.user.email",
	"repo.signing.format",
	"repo.signing.key",
	"repo.signing.commit",
	"repo.signing.tag",
	"repo.signing.allowedSigners",
	"head.present",
	"head.signature",
	"head.signer",
	"head.author",
	"head.committer",
	"head.trailer",
	"head.attribution",
}

// checkGit reports Git availability and version and returns a usable runner.
func checkGit(ctx context.Context, b *builder, opts Options, dir string, policy Policy) *gitcli.Runner {
	git := opts.Git
	if git == nil {
		created, err := gitcli.New(ctx, gitcli.Options{Dir: dir})
		if err != nil {
			b.fail("git.binary", true, err.Error())
			b.skip("git.version", "git is unavailable")
			return nil
		}
		git = created
	}
	b.pass("git.binary", git.Binary())

	version, err := git.Version(ctx)
	if err != nil {
		b.fail("git.version", true, err.Error())
		return git
	}
	// The floor is named with the capability that sets it, because an operator
	// who has to raise a Git version deserves to know what is missing rather
	// than only that a number is too small.
	b.result("git.version", true, version.AtLeast(policy.MinimumGit),
		fmt.Sprintf("%s (minimum %s, which is where GIT_NO_LAZY_FETCH is honoured)", version, policy.MinimumGit))
	return git
}

// checkGo reports Go availability, version, and writable build caches.
func checkGo(ctx context.Context, b *builder, opts Options, policy Policy) {
	binary := opts.GoBinary
	if binary == "" {
		binary = "go"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		b.fail("go.binary", true, fmt.Sprintf("go executable lookup: %v", err))
		b.skip("go.version", "go is unavailable")
		for _, name := range goCacheChecks() {
			b.skip(name, "go is unavailable")
		}
		return
	}
	b.pass("go.binary", resolved)

	out, err := runGo(ctx, resolved, "version")
	if err != nil {
		b.fail("go.version", true, err.Error())
	} else {
		fields := strings.Fields(out)
		if len(fields) < 3 {
			b.fail("go.version", true, fmt.Sprintf("unexpected go version output %q", strings.TrimSpace(out)))
		} else {
			release := strings.TrimPrefix(fields[2], "go")
			version, parseErr := gitcli.ParseVersion(release)
			switch {
			case parseErr != nil:
				b.fail("go.version", true, parseErr.Error())
			case !version.AtLeast(policy.MinimumGo):
				b.fail("go.version", true, fmt.Sprintf("go%s is older than the minimum go%s", release, policy.MinimumGo))
			case fields[2] != policy.Toolchain:
				b.warn("go.version", fmt.Sprintf("%s does not match the pinned toolchain %s", fields[2], policy.Toolchain))
			default:
				b.pass("go.version", fmt.Sprintf("%s matches the pinned toolchain", fields[2]))
			}
		}
	}

	variables := make([]string, 0, len(goCaches))
	for _, cache := range goCaches {
		variables = append(variables, cache.variable)
	}
	values, err := goEnv(ctx, resolved, variables...)
	if err != nil {
		for _, name := range goCacheChecks() {
			b.fail(name, true, err.Error())
		}
		return
	}
	for i, cache := range goCaches {
		detail, ok := writableDetail(ctx, values[i])
		b.result(cache.check, true, ok, detail)
	}
}

// checkRepository reports repository identity, signing configuration, and the
// state of the current HEAD commit.
func checkRepository(ctx context.Context, b *builder, git *gitcli.Runner, policy Policy) {
	isRepo, err := git.IsRepository(ctx)
	if err != nil {
		b.fail("repo.present", true, err.Error())
		b.skipRemaining(repositoryCheckNames, "repository state is unknown")
		return
	}
	if !isRepo {
		b.warn("repo.present", fmt.Sprintf("%s is not a Git repository yet", git.Dir()))
		b.skipRemaining(repositoryCheckNames, "directory is not a repository")
		return
	}
	b.pass("repo.present", git.Dir())

	config := func(name, key, want, hint string, required bool) {
		value, found, err := git.ConfigLocal(ctx, key)
		switch {
		case err != nil:
			b.fail(name, required, err.Error())
		case !found:
			b.result(name, required, false, fmt.Sprintf("%s is not set locally, want %q%s", key, want, hint))
		case value != want:
			b.result(name, required, false, fmt.Sprintf("%s is %q, want %q%s", key, value, want, hint))
		default:
			b.result(name, required, true, fmt.Sprintf("%s=%s", key, value))
		}
	}
	config("repo.user.name", "user.name", policy.AuthorName, "", true)
	config("repo.user.email", "user.email", policy.AuthorEmail, "", true)
	config("repo.signing.format", "gpg.format", "ssh", "", true)
	config("repo.signing.key", "user.signingkey", policy.SigningKeyPath, "", true)
	config("repo.signing.commit", "commit.gpgsign", "true", "", true)
	config("repo.signing.tag", "tag.gpgsign", "true", " so annotated engine tags are signed", true)

	signingKey, keyErr := loadPublicKey(ctx, policy.SigningKeyPath)
	checkAllowedSigners(ctx, b, git, policy, signingKey, keyErr)

	hasHead, err := git.HasHead(ctx)
	if err != nil {
		b.fail("head.present", true, err.Error())
		b.skipRemaining(repositoryCheckNames, "HEAD state is unknown")
		return
	}
	if !hasHead {
		b.pass("head.present", "repository has no commits yet")
		b.skipRemaining(repositoryCheckNames, "repository has no HEAD commit")
		return
	}
	commit, err := git.CommitInfo(ctx, "HEAD")
	if err != nil {
		b.fail("head.present", true, err.Error())
		b.skipRemaining(repositoryCheckNames, "HEAD could not be read")
		return
	}
	b.pass("head.present", commit.SHA)
	checkHead(b, commit, policy, signingKey, keyErr)
}

// checkAllowedSigners reports whether the configured allowed signers file
// authorizes the policy signing key for the policy identity.
func checkAllowedSigners(ctx context.Context, b *builder, git *gitcli.Runner, policy Policy, signingKey publicKey, keyErr error) {
	const name = "repo.signing.allowedSigners"
	configured, found, err := git.ConfigLocal(ctx, "gpg.ssh.allowedsignersfile")
	switch {
	case err != nil:
		b.fail(name, true, err.Error())
		return
	case !found:
		b.fail(name, true, "gpg.ssh.allowedsignersfile is not set locally")
		return
	}
	path, err := resolveConfiguredPath(ctx, git, configured)
	if err != nil {
		b.fail(name, true, err.Error())
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.fail(name, true, fmt.Sprintf("read allowed signers: %v", err))
		return
	}
	if keyErr != nil {
		b.fail(name, true, fmt.Sprintf("read signing key: %v", keyErr))
		return
	}
	if authorizesKey(string(data), policy.AuthorEmail, signingKey) {
		b.pass(name, fmt.Sprintf("%s authorizes %s for %s", configured, signingKey.Fingerprint, policy.AuthorEmail))
		return
	}
	b.fail(name, true, fmt.Sprintf("%s does not authorize %s for %s", configured, signingKey.Fingerprint, policy.AuthorEmail))
}

// resolveConfiguredPath resolves a configuration value that names a file.
// Git resolves a relative allowed signers path against the repository root, so
// the doctor must inspect the same file git would.
func resolveConfiguredPath(ctx context.Context, git *gitcli.Runner, value string) (string, error) {
	expanded := expandHome(value)
	if filepath.IsAbs(expanded) {
		return expanded, nil
	}
	root, err := git.RepositoryRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve %q against the repository root: %w", value, err)
	}
	return filepath.Join(root, filepath.FromSlash(expanded)), nil
}

// authorizesKey reports whether an allowed signers file authorizes key for
// principal.
//
// Each line is "principals [options] keytype base64key [comment]". Principals
// are a comma separated list of patterns that may use * and ?, and a pattern
// prefixed with ! denies. A denial anywhere on a matching line wins, which is
// how OpenSSH reads the file.
func authorizesKey(contents, principal string, key publicKey) bool {
	for line := range strings.SplitSeq(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !lineHasKey(fields[1:], key) {
			continue
		}
		if matchPrincipal(fields[0], principal) {
			return true
		}
	}
	return false
}

// lineHasKey reports whether the fields after the principals carry key. The
// optional options field is skipped by searching for the algorithm rather than
// by counting positions.
func lineHasKey(fields []string, key publicKey) bool {
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == key.Algorithm && fields[i+1] == key.Blob {
			return true
		}
	}
	return false
}

// matchPrincipal applies one comma separated principal pattern list.
func matchPrincipal(patterns, principal string) bool {
	matched := false
	for pattern := range strings.SplitSeq(patterns, ",") {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		negated := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		if !matchPattern(pattern, principal) {
			continue
		}
		if negated {
			return false
		}
		matched = true
	}
	return matched
}

// matchPattern matches one OpenSSH style pattern against a principal. Principals
// carry no slashes, so path.Match implements the same * and ? semantics.
func matchPattern(pattern, principal string) bool {
	if strings.EqualFold(pattern, principal) {
		return true
	}
	ok, err := path.Match(strings.ToLower(pattern), strings.ToLower(principal))
	return err == nil && ok
}

// checkHead reports the signature, identity, and trailers of the HEAD commit.
func checkHead(b *builder, commit gitcli.Commit, policy Policy, signingKey publicKey, keyErr error) {
	b.result("head.signature", true, commit.SignatureStatus == "G",
		fmt.Sprintf("signature status %q (%s)", commit.SignatureStatus, describeSignatureStatus(commit.SignatureStatus)))

	switch {
	case keyErr != nil:
		b.fail("head.signer", true, fmt.Sprintf("read signing key: %v", keyErr))
	case commit.SignerKey == "":
		b.fail("head.signer", true, fmt.Sprintf("commit records no signing key, policy key is %s", signingKey.Fingerprint))
	default:
		b.result("head.signer", true, commit.SignerKey == signingKey.Fingerprint,
			fmt.Sprintf("signed with %s, policy key is %s", commit.SignerKey, signingKey.Fingerprint))
	}

	b.result("head.author", true, commit.AuthorIdentity() == policy.Identity(),
		fmt.Sprintf("author %s, want %s", commit.AuthorIdentity(), policy.Identity()))
	b.result("head.committer", true, commit.CommitterIdentity() == policy.Identity(),
		fmt.Sprintf("committer %s, want %s", commit.CommitterIdentity(), policy.Identity()))

	signoffs := commit.TrailerValues(policy.SignoffTrailerKey)
	matching := 0
	for _, value := range signoffs {
		if value == policy.SignoffTrailerValue {
			matching++
		}
	}
	b.result("head.trailer", true, matching == 1 && len(signoffs) == 1,
		fmt.Sprintf("found %d %s trailers, %d matching %q, want exactly 1",
			len(signoffs), policy.SignoffTrailerKey, matching, policy.SignoffTrailerValue))

	var attribution []string
	for _, trailer := range commit.Trailers {
		if strings.EqualFold(trailer.Key, "Co-authored-by") {
			attribution = append(attribution, trailer.Key+": "+trailer.Value)
		}
	}
	b.result("head.attribution", true, len(attribution) == 0,
		fmt.Sprintf("found %d co-author trailers, want 0", len(attribution)))
}

// describeSignatureStatus explains a git %G? code.
func describeSignatureStatus(code string) string {
	switch code {
	case "G":
		return "good signature"
	case "B":
		return "bad signature"
	case "U":
		return "good signature with unknown validity"
	case "X":
		return "good signature that has expired"
	case "Y":
		return "good signature made by an expired key"
	case "R":
		return "good signature made by a revoked key"
	case "E":
		return "signature could not be checked"
	case "N":
		return "no signature"
	default:
		return "unknown status"
	}
}

// runGo executes one go subcommand. The process environment is passed through
// because the doctor reports what the go command would actually do.
func runGo(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = os.Environ()
	// A descendant that keeps the output pipe open must not be able to hold the
	// run open after cancellation.
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("go %s: exit status %d: %s", strings.Join(args, " "),
				exitErr.ExitCode(), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// goEnv reads Go environment values in the requested order.
func goEnv(ctx context.Context, binary string, names ...string) ([]string, error) {
	out, err := runGo(ctx, binary, append([]string{"env"}, names...)...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimRight(out, "\n"), "\r", ""), "\n")
	if len(lines) != len(names) {
		return nil, fmt.Errorf("go env: got %d values, want %d", len(lines), len(names))
	}
	return lines, nil
}

// writableDetail reports whether a Go cache directory can be written. The
// context is observed between filesystem operations so a cancelled run stops
// probing rather than finishing every check first.
func writableDetail(ctx context.Context, path string) (string, bool) {
	if err := ctx.Err(); err != nil {
		return err.Error(), false
	}
	switch path {
	case "":
		return "not set", false
	case "off":
		return "disabled", false
	}
	if list := filepath.SplitList(path); len(list) > 1 {
		path = list[0]
	}
	if !filepath.IsAbs(path) {
		return fmt.Sprintf("%s is not an absolute path", path), false
	}
	existing := path
	for {
		if err := ctx.Err(); err != nil {
			return err.Error(), false
		}
		if _, err := os.Stat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return fmt.Sprintf("%s has no existing ancestor directory", path), false
		}
		existing = parent
	}
	probe, err := os.CreateTemp(existing, ".soapbox-doctor-")
	if err != nil {
		return fmt.Sprintf("%s is not writable: %v", existing, err), false
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	if err := errors.Join(closeErr, removeErr); err != nil {
		return fmt.Sprintf("%s probe cleanup failed: %v", existing, err), false
	}
	if existing == path {
		return path + " is writable", true
	}
	return fmt.Sprintf("%s is missing and will be created under writable %s", path, existing), true
}

// expandHome expands a leading ~ element using the current user's home
// directory.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}

// builder accumulates checks in a stable order.
type builder struct {
	checks []Check
	seen   map[string]bool
}

func (b *builder) add(check Check) {
	if b.seen == nil {
		b.seen = make(map[string]bool)
	}
	b.seen[check.Name] = true
	b.checks = append(b.checks, check)
}

func (b *builder) pass(name, detail string) {
	b.add(Check{Name: name, Status: StatusPass, Required: true, Detail: detail})
}

func (b *builder) fail(name string, required bool, detail string) {
	status := StatusFail
	if !required {
		status = StatusWarn
	}
	b.add(Check{Name: name, Status: status, Required: required, Detail: detail})
}

func (b *builder) warn(name, detail string) {
	b.add(Check{Name: name, Status: StatusWarn, Required: false, Detail: detail})
}

func (b *builder) skip(name, detail string) {
	b.add(Check{Name: name, Status: StatusSkip, Required: false, Detail: detail})
}

// result records a pass or a failure for a check that ran.
func (b *builder) result(name string, required, ok bool, detail string) {
	if ok {
		b.add(Check{Name: name, Status: StatusPass, Required: required, Detail: detail})
		return
	}
	b.fail(name, required, detail)
}

// skipRemaining records every not yet reported check from names as skipped.
func (b *builder) skipRemaining(names []string, detail string) {
	for _, name := range names {
		if !b.seen[name] {
			b.skip(name, detail)
		}
	}
}

func (b *builder) report() *Report {
	return &Report{Checks: b.checks}
}
