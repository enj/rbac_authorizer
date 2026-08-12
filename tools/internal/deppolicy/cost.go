package deppolicy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// distinctLicenseIdentifiers returns the distinct verified licence identities,
// sorted.
//
// Identity rather than file count is what a licensing obligation is made of. A
// module carrying LICENSE, NOTICE, and PATENTS is under one licence with two
// accompanying documents, and counting files would report three obligations
// where there is one.
func distinctLicenseIdentifiers(licenses []License) []string {
	identifiers := make([]string, 0, len(licenses))
	for _, license := range licenses {
		if identifier := strings.TrimSpace(license.Identifier); identifier != "" {
			identifiers = append(identifiers, identifier)
		}
	}
	slices.Sort(identifiers)
	return slices.Compact(identifiers)
}

// Score is every cost measurement for one candidate.
//
// Every field is measured from the graph and the candidate's own files, never
// asserted by configuration, with the two documented exceptions that only the
// toolchain and Git can answer. The fixtures record what these measurements
// come out as; they do not tell the algorithm what to compute.
type Score struct {
	// CopiedFiles is how many build inputs a copy would take ownership of.
	CopiedFiles int
	// CopiedLines is how many lines of those files the generated module would
	// then maintain, review, and answer for.
	CopiedLines int
	// GeneratedFiles is how many of them are generated. Generated code is the
	// most expensive kind to own, because the generator is not being copied and
	// the next upstream change to the source of truth cannot be regenerated.
	GeneratedFiles int
	// NativeFiles is how many are not Go. They make the generated module's
	// portability a build system question rather than a source question.
	NativeFiles int
	// Cgo reports whether the candidate imports "C", which has the same effect
	// and does not show up as a separate file.
	Cgo bool
	// LicenseIdentifiers are the distinct licence identities the caller
	// verified by reading the module's licensing documents, sorted. A file
	// named LICENSE proves only that a file is named LICENSE, so the identity
	// is supplied rather than inferred and this package never claims a licence
	// it did not read.
	LicenseIdentifiers []string
	// LicensesVerified reports whether the caller supplied that verification.
	LicensesVerified bool
	// SecurityCriticalSegment is the path segment that makes this code decide
	// who may do what, or the empty string. Owning a copy of such code means
	// owning its CVE response.
	SecurityCriticalSegment string
	// ClosureGaps are imports the candidate makes into its own module that are
	// not themselves candidates. Each one is a package the copy would still
	// need, so a non empty list means the proposed copy is not a closure and
	// would not build once relocated.
	ClosureGaps []string
	// ModuleZipBytes is the module zip size the toolchain reports. Supplied
	// rather than computed, because only the toolchain knows it.
	ModuleZipBytes int64
	// ZipBytesKnown reports whether that measurement was supplied.
	ZipBytesKnown bool
	// ReleasesPerMinor is how often the module changed upstream during the
	// source minor series. Supplied rather than computed, because it is a Git
	// fact about another repository. A fast moving dependency is expensive to
	// own: every release becomes a merge the generated module performs itself.
	ReleasesPerMinor int
	// CadenceKnown reports whether that measurement was supplied.
	CadenceKnown bool
	// ModulesRemoved are the modules that would actually leave the consumer
	// build, sorted. This is the benefit side of the ledger, and it is usually
	// empty: copying some packages of a module that stays for the others
	// removes nothing.
	ModulesRemoved []string
	// PackagesRemoved is how many compiled packages would leave the build.
	PackagesRemoved int
	// LinesRemoved is how many compiled lines would leave with them, summed
	// from the caller's per package counts.
	LinesRemoved int
}

// measureCost measures every candidate, including candidates a correctness gate
// already refused.
//
// Refused candidates are measured on purpose. An operator arguing that a
// refusal is wrong needs the numbers behind it, and a fixture that pins this
// decision is only worth having if the numbers in it are real.
func (d *Decider) measureCost(ctx context.Context, graph *Graph) (map[string]Score, error) {
	owned := ownedPackages(graph.Candidates)
	removed := modulesRemoved(graph, d.opts.ModulePath, owned)
	packagesRemoved, linesRemoved := removedTotals(graph, removed, owned)

	scores := make(map[string]Score, len(graph.Candidates))
	for _, candidate := range graph.Candidates {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("measure candidate cost: %w", err)
		}
		score, err := measureCandidate(ctx, graph, candidate, owned)
		if err != nil {
			return nil, err
		}
		// The build wide benefit is reported against every candidate because
		// the benefit is a property of the whole proposal, not of one package
		// in it. Splitting it per candidate would invite the arithmetic this
		// policy refuses to do.
		score.ModulesRemoved = removed
		score.PackagesRemoved = packagesRemoved
		score.LinesRemoved = linesRemoved
		scores[candidate.Package.ImportPath] = score
	}
	return scores, nil
}

// measureCandidate reads one candidate's files and its module root.
func measureCandidate(ctx context.Context, graph *Graph, candidate Candidate, owned map[string]bool) (Score, error) {
	pkg := candidate.Package
	score := Score{
		SecurityCriticalSegment: securityCriticalSegment(pkg.ImportPath),
		ClosureGaps:             closureGaps(pkg, owned),
		Cgo:                     importsCgo(pkg),
	}

	for _, name := range pkg.OtherFiles {
		if isNativeFile(name) {
			score.NativeFiles++
		}
	}
	score.CopiedFiles = len(pkg.GoFiles) + len(pkg.OtherFiles)

	if pkg.Dir != "" && len(pkg.GoFiles) > 0 {
		root, err := os.OpenRoot(pkg.Dir)
		if err != nil {
			return Score{}, &CandidateError{StagingPath: candidate.StagingPath,
				Err: fmt.Errorf("%w: open %s: %w", ErrMeasureFailed, pkg.Dir, err)}
		}
		defer root.Close()

		for _, name := range slices.Sorted(slices.Values(pkg.GoFiles)) {
			if err := ctx.Err(); err != nil {
				return Score{}, fmt.Errorf("measure candidate %s: %w", candidate.StagingPath, err)
			}
			data, err := root.ReadFile(path.Base(name))
			if err != nil {
				// A cost that cannot be measured is never recorded as zero. A
				// missing file is refused, because admitting a copy on absent
				// evidence is exactly the failure this policy exists to stop.
				return Score{}, &CandidateError{StagingPath: candidate.StagingPath,
					Err: fmt.Errorf("%w: read %s: %w", ErrMeasureFailed, name, err)}
			}
			score.CopiedLines += countLines(data)
			// Generated file classification is rewrite's, not a second byte
			// scan here. The two must agree, because the rewrite phase decides
			// which files carry a modification notice and this gate decides how
			// expensive owning them is, and a disagreement would mean the
			// engine counted one set of files and rewrote another.
			if rewrite.Generated(data) {
				score.GeneratedFiles++
			}
		}
	}

	// Module facts the caller did not supply stay unknown. A module missing
	// from the graph leaves every one of them unknown, which refuses the gates
	// that depend on them rather than scoring them as zero.
	if module, ok := graph.module(pkg.Module); ok {
		score.ModuleZipBytes = module.ZipBytes
		score.ZipBytesKnown = module.ZipBytesKnown
		score.ReleasesPerMinor = module.ReleasesPerMinor
		score.CadenceKnown = module.CadenceKnown
		score.LicensesVerified = module.LicensesVerified
		score.LicenseIdentifiers = distinctLicenseIdentifiers(module.Licenses)
	}
	return score, nil
}

// closureGaps lists the candidate's imports into its own module that are not
// themselves candidates.
//
// Imports into a different module are not gaps: those stay external, which is
// the default outcome and needs no copying. An import into the same module is a
// gap because the relocated copy would reference a package that did not move
// with it.
func closureGaps(pkg *Package, owned map[string]bool) []string {
	var gaps []string
	for _, imported := range pkg.Imports {
		if owned[imported] || imported == pkg.ImportPath {
			continue
		}
		if pkg.Module != "" && (imported == pkg.Module || strings.HasPrefix(imported, pkg.Module+"/")) {
			gaps = append(gaps, imported)
		}
	}
	slices.Sort(gaps)
	return slices.Compact(gaps)
}

// importsCgo reports whether any of the candidate's files imports "C".
func importsCgo(pkg *Package) bool {
	for _, file := range pkg.Syntax {
		for _, spec := range file.Imports {
			if spec.Path != nil && spec.Path.Value == `"C"` {
				return true
			}
		}
	}
	return false
}

// countLines counts the lines in a file, counting a final unterminated line.
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := bytes.Count(data, []byte("\n"))
	if !bytes.HasSuffix(data, []byte("\n")) {
		lines++
	}
	return lines
}

// removedTotals counts the packages and lines that would leave the build with
// the removed modules.
func removedTotals(graph *Graph, removedModules []string, owned map[string]bool) (int, int) {
	var packages, lines int
	for _, pkg := range graph.Build {
		if owned[pkg.ImportPath] || pkg.Module == "" {
			continue
		}
		if !slices.Contains(removedModules, pkg.Module) {
			continue
		}
		packages++
		lines += pkg.Lines
	}
	return packages, lines
}
