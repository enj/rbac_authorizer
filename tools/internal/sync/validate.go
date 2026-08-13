package sync

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/publish"
)

// checkDestination proves the destination is the one the profile names.
//
// A synchronization writes a tag object claiming an upstream release and a
// state record claiming a repository, and both are append only once published.
// Nothing downstream can tell that they went somewhere else: the publisher
// checks that the refs are well formed and the namespaces are respected, not
// that the repository is the right repository. So the profile is the authority
// and this is where it is applied.
//
// A local remote is the exception the dry run needs, and it is a narrow one.
// The location may be a temporary directory, because that is what a rehearsal
// publishes into, but the identity may not: it is what the manifest records and
// what an approval is read against, so a local rehearsal has to state the same
// canonical repository the real publication would.
func checkDestination(dest Destination, cfg *config.Config) error {
	if dest.Remote == "" {
		return fmt.Errorf("%w: no remote was configured", ErrPublicationDisabled)
	}
	want := canonicalIdentity(cfg.Destination.Repository)
	if want == "" {
		return fmt.Errorf("synchronization: the profile names no destination repository")
	}
	local := isLocalRemote(dest.Remote)
	if local && !dest.AllowLocalRemote {
		// The publisher's own sentinel is used rather than a second name for the
		// same condition, because a caller testing for a refused filesystem
		// destination should not have to know which layer noticed first.
		return fmt.Errorf(
			"synchronization: %w: %s is a filesystem path, which only an explicit local rehearsal may publish to",
			publish.ErrLocalRemoteNotAllowed, redactRemote(dest.Remote))
	}
	if local {
		// The identity is stated rather than derived, so it is the one thing a
		// rehearsal can get wrong without anything noticing. It has to be the
		// repository the profile names, or the manifest an operator approves
		// describes a publication that was never rehearsed.
		if dest.Identity != want {
			return fmt.Errorf(
				"synchronization: a local rehearsal records identity %q, and the profile names %q",
				dest.Identity, want)
		}
		return nil
	}
	if dest.Remote != cfg.Destination.Remote {
		return fmt.Errorf(
			"synchronization: the destination remote %s is not the profile's %s",
			redactRemote(dest.Remote), redactRemote(cfg.Destination.Remote))
	}
	if dest.Identity != "" && dest.Identity != want {
		return fmt.Errorf(
			"synchronization: the destination identity %q is not the profile's %q", dest.Identity, want)
	}
	return nil
}

// canonicalIdentity renders the repository the manifest records.
func canonicalIdentity(repository string) string {
	if repository == "" {
		return ""
	}
	return "github.com/" + repository
}

// isLocalRemote reports a destination that lives on this machine.
//
// The publisher decides this too, and decides it authoritatively; this is the
// same question asked earlier so that a refusal happens before any object is
// written. The rule is deliberately the narrow one: a publication host remote
// is an https URL, and anything else is somewhere on this filesystem. A remote
// that is neither reaches the publisher and is refused there by name.
func isLocalRemote(remote string) bool {
	return !strings.HasPrefix(remote, "https://")
}

// redactRemote renders a remote without the parts a credential rides in.
func redactRemote(remote string) string {
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Scheme == "" {
		// A path rather than a URL. Its last element is enough to recognise it,
		// and the directories above it are the operator's business.
		return "a local path"
	}
	if parsed.User != nil {
		parsed.User = url.User("redacted")
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String()
}

// checkModuleAgrees proves the generation and the release describe one release.
//
// The report is what the manifest summarizes and the release is what the tag
// object records, and they arrive from different places: a caller composes both
// for Project, and Plan reads one from a generation and the other from that
// generation's source cache. Nothing downstream compares them. A report
// generated from one commit paired with a release read for another produces a
// tag naming a commit whose module was never built, which is the exact failure
// no later gate can see, because every object involved is internally
// consistent.
func checkModuleAgrees(report generate.Report, rel Release, cfg *config.Config) error {
	for _, field := range []struct{ what, got, want string }{
		{"upstream commit", report.Source.Commit, rel.Commit},
		{"upstream ref name", report.Source.RefName, rel.Tag},
		{"destination module", report.Output.Module, cfg.Destination.Module},
		{"toolchain", report.Engine.Toolchain, cfg.Determinism.Toolchain},
	} {
		if field.got != field.want {
			return fmt.Errorf(
				"synchronization: the generated module reports %s %q and this run publishes %q",
				field.what, field.got, field.want)
		}
	}
	// The release tag is the policy's own mapping, so it is recomputed rather
	// than believed. A report carrying a tag the policy would not produce is a
	// report from a different profile.
	mapped, err := config.MapReleaseTag(cfg.Release.Policy, rel.Tag)
	if err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}
	if report.Source.ReleaseTag != mapped {
		return fmt.Errorf(
			"synchronization: the generated module reports release tag %q and the %s policy maps %s onto %q",
			report.Source.ReleaseTag, cfg.Release.Policy, rel.Tag, mapped)
	}
	if report.Engine.ProfileHash == "" {
		return fmt.Errorf("synchronization: the generated module reports no profile hash")
	}
	// A generation that read its source from somewhere other than the profile's
	// repository produced a module whose provenance cannot be stated. The tag
	// message names the profile's release page, so publishing one for an
	// overridden source would put a URL in an immutable object claiming a
	// release that this module was not built from.
	if report.Source.RemoteOverridden {
		return fmt.Errorf(
			"%w: the module was generated from an overridden source remote, so a tag claiming %s would be a false provenance record",
			ErrUnsupported, cfg.Source.Repository)
	}
	return nil
}

// checkManifestStrings proves nothing a machine chose reached the manifest.
//
// Every string here originates in the generation report, which is built to
// carry no path. "Built to" is the problem: the manifest is the artifact an
// approval names and a record of what was authorized, and a report that grew a
// path in some phase would put it there without anything noticing. The check is
// cheap, it runs once, and it is the difference between a promise made in
// another package and a property this one holds.
//
// A newline is refused for a different reason. The text rendering is compared
// line by line, so a notice spanning two lines would let one generation decide
// how many lines an approval occupies.
func checkManifestStrings(m Manifest) error {
	groups := []struct {
		what   string
		values []string
	}{
		{"notice", m.Module.Notices},
		{"pruned file", m.Module.PrunedFiles},
		{"denied import", m.Module.DeniedImports},
		{"public API name", m.Module.PublicAPI},
		{"copied dependency", m.Module.Dependencies.Copy},
	}
	for _, change := range m.Module.BehaviorChanges {
		groups = append(groups, struct {
			what   string
			values []string
		}{"behavior change", []string{change.Summary, change.Cause}})
	}
	for _, action := range m.Publish.Actions {
		groups = append(groups, struct {
			what   string
			values []string
		}{"evidence", []string{action.Evidence}})
	}
	for _, group := range groups {
		for _, value := range group.values {
			if err := checkOpaqueString(group.what, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkOpaqueString refuses one manifest string that names a machine.
func checkOpaqueString(what, value string) error {
	switch {
	case strings.HasPrefix(value, "/"), strings.HasPrefix(value, `\`):
		return fmt.Errorf("%w: the %s %q is an absolute path", ErrManifestLocation, what, value)
	case strings.Contains(value, "://"):
		return fmt.Errorf("%w: the %s %q carries a URL", ErrManifestLocation, what, value)
	case hasDriveLetter(value):
		return fmt.Errorf("%w: the %s %q is an absolute path", ErrManifestLocation, what, value)
	}
	for _, r := range value {
		if r == '\n' || r == '\r' || r == 0 || (r < 0x20 && r != '\t') {
			return fmt.Errorf("%w: the %s %q carries a control character", ErrManifestLocation, what, value)
		}
	}
	return nil
}

// hasDriveLetter reports a Windows absolute path, which is a path this engine
// would otherwise not recognise as one.
func hasDriveLetter(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	c := value[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
