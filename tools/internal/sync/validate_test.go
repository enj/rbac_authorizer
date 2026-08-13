package sync_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/sync"
)

// TestProjectRefusesADestinationTheProfileDoesNotName is the gate that keeps a
// synchronization from writing into somebody else's repository.
//
// Nothing downstream can catch this. The publisher checks that refs are well
// formed and that namespaces are respected, not that the repository is the
// right one, so a mistyped remote would publish a real release tag into a real
// repository and every object involved would be internally consistent.
func TestProjectRefusesADestinationTheProfileDoesNotName(t *testing.T) {
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		mutate func(*sync.Destination)
		want   error
	}{{
		name: "a network remote that is not the profile's",
		mutate: func(d *sync.Destination) {
			d.Remote, d.AllowLocalRemote = "https://github.com/attacker/rbac.git", false
		},
	}, {
		name:   "a rehearsal claiming a repository the profile does not name",
		mutate: func(d *sync.Destination) { d.Identity = "github.com/someone/else" },
	}, {
		name:   "a rehearsal claiming no repository at all",
		mutate: func(d *sync.Destination) { d.Identity = "" },
	}, {
		name:   "a filesystem destination nobody permitted",
		mutate: func(d *sync.Destination) { d.AllowLocalRemote = false },
		want:   publish.ErrLocalRemoteNotAllowed,
	}, {
		name:   "no remote at all",
		mutate: func(d *sync.Destination) { d.Remote = "" },
		want:   sync.ErrPublicationDisabled,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dest := newDestination(ctx, t)
			opts := dest.options()
			tc.mutate(&opts.Destination)

			_, err := sync.Project(ctx, opts)
			if err == nil {
				t.Fatalf("project succeeded, want a refused destination")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("project = %v, want %v", err, tc.want)
			}
			assertNothingWritten(ctx, t, dest)
		})
	}
}

// TestProjectRefusesAModuleThatDescribesAnotherRelease proves the two halves of
// a synchronization are checked against each other.
//
// The report is what the manifest summarizes; the release is what the tag
// object records. They arrive from different places, and nothing downstream
// compares them, so a report built from one commit paired with a release read
// for another would produce a tag naming a commit whose module was never built.
func TestProjectRefusesAModuleThatDescribesAnotherRelease(t *testing.T) {
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		mutate func(*sync.ProjectOptions)
	}{{
		name: "a report from another commit",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Source.Commit = strings.Repeat("7", 40)
			o.Module.Report = report
		},
	}, {
		name: "a report from another upstream tag",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Source.RefName = "v1.35.0"
			o.Module.Report = report
		},
	}, {
		name: "a release tag the policy would not produce",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Source.ReleaseTag = "v0.99.0"
			o.Module.Report = report
		},
	}, {
		name: "a module path that is not the profile's",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Output.Module = "example.com/somewhere/else"
			o.Module.Report = report
		},
	}, {
		name: "a toolchain the profile does not pin",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Engine.Toolchain = "go1.21.0"
			o.Module.Report = report
		},
	}, {
		name: "no profile hash at all",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Engine.ProfileHash = ""
			o.Module.Report = report
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			dest := newDestination(ctx, t)
			opts := dest.options()
			tc.mutate(&opts)

			if _, err := sync.Project(ctx, opts); err == nil {
				t.Fatalf("project succeeded, want a refused module")
			}
			assertNothingWritten(ctx, t, dest)
		})
	}
}

// TestProjectRefusesAModuleGeneratedFromAnOverriddenSource is the provenance
// gate.
//
// The destination tag message names the profile's upstream release page, and a
// tag object can never be taken back. A module generated from a local mirror or
// a fork is not the release that page describes, so publishing one would put a
// false provenance claim into an immutable object. The override is legitimate
// for testing a generation; it is not legitimate for publishing one.
func TestProjectRefusesAModuleGeneratedFromAnOverriddenSource(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	opts := dest.options()
	report := opts.Module.Report
	report.Source.RemoteOverridden = true
	opts.Module.Report = report

	_, err := sync.Project(ctx, opts)
	if !errors.Is(err, sync.ErrUnsupported) {
		t.Fatalf("project = %v, want an unsupported run shape", err)
	}
	assertNothingWritten(ctx, t, dest)
}

// TestProjectRefusesANetworkDestinationItCannotRead proves the lister guard
// asks about the remote rather than about a flag.
//
// A run that permitted local remotes and then named an https destination used
// to fall through to the local reader, which would fail much later with a path
// parsing error about a URL. What decides whether this engine can read a
// destination is where the destination is, not what the caller allowed.
func TestProjectRefusesANetworkDestinationItCannotRead(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	opts := dest.options()
	opts.Destination.Remote = testConfig().Destination.Remote
	opts.Destination.Identity = testIdentity
	// The permission is left on, which is exactly the combination that used to
	// slip through: allowed local remotes plus a network destination.
	opts.Destination.AllowLocalRemote = true
	opts.Destination.Lister = nil

	_, err := sync.Project(ctx, opts)
	if !errors.Is(err, sync.ErrPublicationDisabled) {
		t.Fatalf("project = %v, want a disabled publication", err)
	}
	if !errors.Is(err, publish.ErrRemoteRefsUnsupported) {
		t.Fatalf("project = %v, want it to name the unreadable remote", err)
	}
	assertNothingWritten(ctx, t, dest)
}

// TestManifestRefusesStringsThatNameAMachine proves the approval artifact is
// checked rather than trusted.
//
// Every free-form string in the manifest comes from the generation report,
// which is built to carry no path. The manifest is compared between machines
// and kept as the record of what was authorized, so "built to" is not enough: a
// report that grew a path in some phase would put it there with nothing
// noticing.
func TestManifestRefusesStringsThatNameAMachine(t *testing.T) {
	ctx := t.Context()

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"an absolute path", "/home/someone/checkout/pkg/rbac.go"},
		{"a windows path", `C:\work\checkout\rbac.go`},
		{"a URL", "cloned from https://github.com/someone/fork.git"},
		{"a newline", "the first line\nand a second one"},
		{"a carriage return", "a line\rand another"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := newDestination(ctx, t)
			opts := dest.options()
			report := opts.Module.Report
			report.Notices = append(report.Notices, tc.value)
			opts.Module.Report = report

			if _, err := sync.Project(ctx, opts); !errors.Is(err, sync.ErrManifestLocation) {
				t.Fatalf("project = %v, want a refused manifest string", err)
			}
		})
	}
}

// assertNothingWritten proves a refused run left the destination alone.
//
// The remote is the part that matters: unreachable objects in the local
// repository cost disk and nothing else, but a ref is a publication.
func assertNothingWritten(ctx context.Context, t *testing.T, dest *destination) {
	t.Helper()
	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after a refused run, want nothing", refs)
	}
}
