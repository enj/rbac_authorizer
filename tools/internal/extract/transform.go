package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/patchset"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

// goParseMode is how a final file is reparsed for the report.
//
// Comments are kept because a Go file's directives are comments, and object
// resolution is skipped because nothing here asks what an identifier refers to.
const goParseMode = parser.ParseComments | parser.SkipObjectResolution

// provenanceDirName is the module relative directory, below the internal
// prefix, that holds a provenance record displaced out of its own package.
//
// A record normally sits beside the package it describes, which is where a
// reader looks for it. It cannot sit there when the package embeds a pattern
// that would capture it, because a generated file appearing inside an embedded
// asset would change what the published module serves. The fallback directory
// is named in capitals after the record itself so it is recognisably this
// engine's rather than upstream's, and the run refuses outright if a real
// package ever claims that name.
//
// The obvious spelling for a directory nothing builds is a dot or underscore
// prefix, and neither is available: the repository's own path validator refuses
// a path element starting with a dot, and an underscore prefixed directory is
// excluded from a module zip by some tooling, which would drop the evidence
// this file exists to carry. The name is proved unembeddable instead of being
// assumed so, in [run.assertProvenanceUnembedded].
const provenanceDirName = "SOAPBOX_PROVENANCE"

// rewriteGenerated reports whether one copy plan entry carries the Go generated
// file marker.
//
// Only Go source is asked. The convention is defined for Go files, and asking it
// of an asset would parse every embedded data file to answer a question that has
// no meaning for it. A .go file that arrived as embedded data or as a matched
// asset is an asset: its bytes are content the published module serves, so the
// marker convention says nothing about it and the answer is no.
func rewriteGenerated(entry closure.CopyEntry, contents []byte) bool {
	if !isGoSourceKind(entry.Kind) {
		return false
	}
	return rewrite.Generated(contents)
}

// isGoSourceKind reports the copy kinds that are Go source of the generated
// module rather than data it carries.
//
// The distinction is the whole of the rule. A file named foo.go under a
// testdata directory, or matched by an asset glob, is bytes the module ships:
// parsing it, relocating its imports, reformatting it, or inserting a
// modification notice into it would change content the module is supposed to
// serve unchanged, and half of those files do not even parse as Go.
func isGoSourceKind(kind closure.CopyKind) bool {
	return kind == closure.KindGo || kind == closure.KindGoTest
}

// isGoSource reports whether one upstream path was selected as Go source.
func (r *run) isGoSource(sourcePath string) bool {
	kind, known := r.kinds[sourcePath]
	return known && isGoSourceKind(kind)
}

// baseRewriteOptions is the transformation policy every file shares.
//
// The upstream repository is the profile's rather than any override, because a
// modification notice and a provenance record name where the code came from, and
// a local mirror is where this run happened to read it. Recording the mirror
// would put a machine's path into a committed file and would make two plans over
// one commit produce different bytes.
//
// Line endings are rejected rather than preserved: the pinned gofmt normalises
// them, so a carriage return that survived the rewrite would still show up as a
// formatting difference in the generated module, and the run has to fail where
// the cause is visible.
func (r *run) baseRewriteOptions() rewrite.Options {
	return rewrite.Options{
		SourcePrefix:      r.cfg.Source.ImportPrefix,
		DestinationModule: r.cfg.Destination.Module,
		InternalPrefix:    r.cfg.Destination.InternalPrefix,
		SourceRepository:  r.cfg.Source.Repository,
		SourceSHA:         r.revision.Commit,
		LineEndings:       rewrite.LineEndingReject,
		Directives:        rewrite.DefaultRules(),
	}
}

// fileRewriteOptions adds the marker policy for one file.
//
// The dangling callback has to be built per file because a marker's evidence is
// its own package: whether the output its generator writes was pruned is a
// question about the directory the marker sits in, and the rules the rewriting
// package evaluates carry the directive but not the file.
func (r *run) fileRewriteOptions(file relocate.File) rewrite.Options {
	opts := r.baseRewriteOptions()
	rules := rewrite.DefaultRules()
	rules.Dangling = func(directive rewrite.Directive) bool {
		if r.markerDangles(directive, file.SourcePackage) {
			return true
		}
		// The callback is consulted for exactly the markers a rule would strip
		// when their target is gone, so one that survives is a generator marker
		// in a module that ships no generators. It is kept, because keeping is
		// what the rules say, and it is recorded, because a reader has to be
		// able to find every instruction the generated module cannot carry out.
		r.notices = append(r.notices, fmt.Sprintf("retained inert generator marker +%s at %s:%d",
			directive.Key, file.Path, directive.Line))
		if _, known := generatedOutputs[directive.Key]; !known {
			// The output table is evidence, not a catalogue. A marker outside it
			// was checked against its value alone, so the second half of the
			// dangling rule never ran, and saying so is what keeps a silent
			// table gap from reading as a clean answer.
			r.notices = append(r.notices, fmt.Sprintf(
				"generator marker +%s at %s:%d names no known output file, so only its value was checked",
				directive.Key, file.Path, directive.Line))
		}
		return false
	}
	opts.Directives = rules
	return opts
}

// markerDangles reports whether a generator marker points at something the
// generated module does not hold.
//
// Two independent pieces of evidence answer it, and nothing else does. The first
// is the marker's own value: an element naming a package below the source prefix
// that the settled closure does not contain describes an input that is gone. The
// second is the marker's output: a generator writes a known file beside the types
// it describes, and a run that removed that file has already decided the
// generator's result is not part of the module.
//
// The second question is asked of the settled tree rather than of the profile's
// prune list. A patch may delete a generated file, and a marker whose output a
// patch deleted is exactly as dangling as one whose output the profile pruned;
// consulting only the prune list would leave the module carrying an instruction
// nothing can carry out. The file has to have existed for its absence to mean
// anything, which is why the comparison is against what this run removed rather
// than against the whole of the final tree.
//
// Anything the two do not cover is kept. A marker whose value names an external
// package, an API group, or free text is not evidence of anything, and stripping
// it on suspicion would quietly change what upstream said about the code.
func (r *run) markerDangles(directive rewrite.Directive, sourcePackage string) bool {
	for _, element := range strings.Split(directive.Value, ",") {
		target := strings.TrimSpace(element)
		dir, inside := sourceRelative(r.cfg.Source.ImportPrefix, target)
		if inside && !r.closureDirs[dir] {
			return true
		}
	}
	output, known := generatedOutputs[directive.Key]
	return known && r.removed[path.Join(sourcePackage, output)]
}

// sourceRelative maps an import path inside the source module onto its
// repository relative directory.
//
// The match is on a path boundary, so a module named k8s.io/kubernetes-extra is
// not read as living inside k8s.io/kubernetes.
func sourceRelative(prefix, importPath string) (string, bool) {
	switch {
	case importPath == prefix:
		return "", true
	case strings.HasPrefix(importPath, prefix+"/"):
		return strings.TrimPrefix(importPath, prefix+"/"), true
	default:
		return "", false
	}
}

// rewriteFiles transforms every relocated file.
func (r *run) rewriteFiles(ctx context.Context, pass1 relocate.FileSet) error {
	r.results = make(map[string]rewrite.Result, len(pass1.Files))

	for _, file := range pass1.Files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("plan rewrite: %w", err)
		}
		input := rewrite.File{
			Path:       file.Path,
			SourcePath: file.Source,
			Contents:   file.Contents,
			Generated:  file.Generated,
		}
		var result rewrite.Result
		var err error
		switch {
		case r.isGoSource(file.Source):
			result, err = rewrite.GoFile(ctx, input, r.fileRewriteOptions(file))
		case path.Ext(file.Path) == ".proto":
			// A .proto reaches the copy plan as a companion the profile matched
			// rather than as Go source, and its import paths still name the
			// upstream module, so it is relocated on its extension.
			result, err = rewrite.ProtoFile(ctx, input, r.baseRewriteOptions())
		default:
			// A header, an object, an embedded asset, or a matched data file has
			// no import to relocate. Passing the bytes through untouched is the
			// whole transformation, and for a .go file that arrived as data it is
			// the only correct one.
			result = rewrite.Result{Contents: file.Contents}
		}
		if err != nil {
			return rewritePolicy(err)
		}
		r.results[file.Path] = result
	}
	return nil
}

// finalContents reports one destination's bytes after transformation.
func (r *run) finalContents(destination string) []byte {
	return r.results[destination].Contents
}

// rewritePolicy classifies a transformation failure.
//
// Every sentinel the rewriting package defines describes the source it was given
// or the configuration it was given, so all of them are findings rather than
// engine faults. The shape and comment checks are included deliberately: a
// rewrite that changed something it was not allowed to change is a bug in the
// engine, but it is one the operator must see as a refusal to publish rather
// than as a crash, and it stops the run either way.
func rewritePolicy(err error) error {
	return policyIf("plan rewrite", err, func(err error) bool {
		return errors.Is(err, rewrite.ErrPrefix) ||
			errors.Is(err, rewrite.ErrCarriageReturn) ||
			errors.Is(err, rewrite.ErrMixedLineEndings) ||
			errors.Is(err, rewrite.ErrShapeChanged) ||
			errors.Is(err, rewrite.ErrCommentsChanged) ||
			errors.Is(err, rewrite.ErrOverlappingEdits) ||
			errors.Is(err, rewrite.ErrEmbedUnmatched) ||
			errors.Is(err, rewrite.ErrEmbedEscape) ||
			errors.Is(err, rewrite.ErrEmbedPattern) ||
			// A file that will not parse reaches here as a plain parse error
			// from go/parser rather than as a sentinel.
			isParseError(err)
	})
}

// isParseError reports a Go syntax failure, which go/parser returns as a
// scanner error list rather than as a typed sentinel any package exports.
func isParseError(err error) bool {
	var list scanner.ErrorList
	return errors.As(err, &list)
}

// measureGoFiles reparses, formats, and verifies the embeds of every final Go
// file.
//
// It runs over the tree the plan would publish rather than over the first
// relocation pass, so the file list a go:embed pattern is verified against is
// the one the module will actually hold, provenance records included. The
// measurements themselves run over the bytes that would ship rather than over
// the ones the rewriting step returned for a file it did not change, so an
// upstream file that was already unformatted is reported as such instead of
// being credited to a transformation that never happened.
func (r *run) measureGoFiles(ctx context.Context) error {
	present := destinationPaths(r.tree)

	for _, file := range r.tree.Files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("plan rewrite: %w", err)
		}
		if !r.isGoSource(file.Source) {
			continue
		}
		r.report.Rewrite.GoFiles++

		if _, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Contents, goParseMode); err != nil {
			r.report.Rewrite.Unparsed = append(r.report.Rewrite.Unparsed, file.Path)
			return policyf("plan rewrite", "%s does not parse after relocation: %w", file.Path, err)
		}
		formatted, err := format.Source(file.Contents)
		if err != nil {
			return policyf("plan rewrite", "%s cannot be formatted: %w", file.Path, err)
		}
		if !bytes.Equal(formatted, file.Contents) {
			// Relocating an import can move it within its group, because the
			// destination module sorts differently from k8s.io/kubernetes. That
			// is a real formatting difference in a module the plan promises to
			// publish gofmt clean, so it is recorded and -strict refuses it.
			r.report.Rewrite.Unformatted = append(r.report.Rewrite.Unformatted, file.Path)
			r.notices = append(r.notices, "the pinned gofmt would reformat "+file.Path)
		}

		embeds, err := rewrite.VerifyEmbeds(ctx, rewrite.File{Path: file.Path, Contents: file.Contents}, present)
		if err != nil {
			return rewritePolicy(err)
		}
		for _, embed := range embeds {
			r.report.Rewrite.Embeds = append(r.report.Rewrite.Embeds, EmbedReport{
				Path:     file.Path,
				Line:     embed.Line,
				Patterns: nonNil(embed.Patterns),
				Matches:  nonNil(embed.Matches),
			})
		}
	}
	return r.assertProvenanceUnembedded()
}

// assertProvenanceUnembedded proves no published embed captured a record this
// engine generated.
//
// Placement already moved every record a pattern would have captured, so this
// never fires. It exists because the consequence of getting the placement wrong
// is invisible: the module would build, serve the record as part of an embedded
// asset, and change what a consumer reads. A cheap proof over the measured
// matches is worth more than the argument that the placement rule is complete.
func (r *run) assertProvenanceUnembedded() error {
	for _, embed := range r.report.Rewrite.Embeds {
		for _, match := range embed.Matches {
			if r.provenance[match] {
				return policyf("plan provenance",
					"the go:embed directive at %s:%d captures the generated record %s",
					embed.Path, embed.Line, match)
			}
		}
	}
	return nil
}

// destinationPaths lists a relocated set's destination paths in its own sorted
// order.
func destinationPaths(set relocate.FileSet) []string {
	out := make([]string, 0, len(set.Files))
	for _, file := range set.Files {
		out = append(out, file.Path)
	}
	return out
}

// provenanceRecord is one closure package's generated provenance file.
type provenanceRecord struct {
	// pkg is the upstream package directory the record describes.
	pkg string
	// source is the upstream relative path the record is relocated from, which
	// decides where it lands.
	source string
	// files are the destination paths of the files it accounts for, sorted.
	files []string
	// displaced reports a record moved out of its own package directory.
	displaced bool
}

// planProvenance decides where every package's provenance record goes and
// renders it.
//
// Records are generated for the closure's packages and for nothing else. A
// package that carries embedded data or a matched asset in a subdirectory
// relocates that file into a directory of its own, and that directory is not a
// package: writing a record into it would invent a package the closure never
// contained and would put a generated file inside a directory the module serves
// as data.
func (r *run) planProvenance(ctx context.Context, pass1 relocate.FileSet) ([]relocate.PlanFile, error) {
	records, err := r.attributeFiles(pass1)
	if err != nil {
		return nil, err
	}
	captured, err := r.capturedProvenance(ctx, pass1, records)
	if err != nil {
		return nil, err
	}

	r.provenance = make(map[string]bool, len(records))
	r.records = make([]*rewrite.PackageProvenance, 0, len(records))
	files := make([]relocate.PlanFile, 0, len(records))
	for i := range records {
		record := &records[i]
		if captured[r.destinationOf(record.source)] {
			record.displaced = true
			record.source = centralProvenancePath(record.pkg)
			r.notices = append(r.notices, fmt.Sprintf(
				"a go:embed pattern in %s would capture its provenance record, so the record was written to %s",
				record.pkg, r.destinationOf(record.source)))
		}
		r.provenance[r.destinationOf(record.source)] = true

		rendered, err := r.render(*record, pass1)
		if err != nil {
			return nil, err
		}
		r.records = append(r.records, rendered)
		files = append(files, relocate.PlanFile{
			Path:    record.source,
			Package: path.Dir(record.source),
			Mode:    relocate.ModeRegular,
			// The record is not upstream source and carries no generated file
			// marker: it is written by this engine and describes the package.
			Contents: []byte(rendered.Render()),
		})
	}
	return files, nil
}

// attributeFiles assigns every relocated file to the closure package that owns
// it.
//
// Ownership is the longest closure package directory that is an ancestor of the
// file's own directory, which is what makes an embedded asset below a package
// belong to that package rather than to the directory it happens to sit in. A
// file with no such ancestor is left unattributed rather than inventing an owner
// for it: a configured asset glob may legitimately match a file outside every
// package, and silently filing it under an unrelated package would put a false
// claim into a record whose only purpose is to be true.
func (r *run) attributeFiles(pass1 relocate.FileSet) ([]provenanceRecord, error) {
	if r.closureDirs[provenanceDirName] {
		return nil, policyf("plan provenance",
			"the closure holds a package at %s, which is the directory reserved for displaced provenance records",
			provenanceDirName)
	}

	owned := make(map[string][]string, len(r.closureResult.Packages))
	for _, file := range pass1.Files {
		owner, found := r.owningPackage(file.SourcePackage)
		if !found {
			r.notices = append(r.notices, fmt.Sprintf(
				"%s lies outside every closure package, so no provenance record accounts for it", file.Source))
			continue
		}
		owned[owner] = append(owned[owner], file.Path)
	}

	records := make([]provenanceRecord, 0, len(r.closureResult.Packages))
	for _, pkg := range r.closureResult.Packages {
		files := owned[pkg.Dir]
		slices.Sort(files)
		records = append(records, provenanceRecord{
			pkg:    pkg.Dir,
			source: path.Join(pkg.Dir, rewrite.ProvenanceFileName),
			files:  files,
		})
	}
	// The closure lists packages by import path while records are written by
	// directory, and the two orders agree only while every package sits at its
	// import path. Sorting here keeps the generated set independent of that.
	slices.SortFunc(records, func(a, b provenanceRecord) int { return strings.Compare(a.pkg, b.pkg) })
	return records, nil
}

// owningPackage reports the closure package directory that owns a relocated
// file's upstream directory.
func (r *run) owningPackage(dir string) (string, bool) {
	for candidate := dir; ; candidate = path.Dir(candidate) {
		if r.closureDirs[candidate] {
			return candidate, true
		}
		if candidate == "." || candidate == "/" || candidate == "" {
			return "", false
		}
	}
}

// capturedProvenance reports the provenance destinations a published go:embed
// pattern would match.
//
// The question has to be asked before the records are placed, and answering it
// needs the file list they would produce, so the patterns are resolved against
// the first pass plus every record's preferred destination. That list is a
// superset of the final one: displacing a record only removes a path from a
// package directory, and the directory records move to cannot be reached by any
// pattern, which [run.assertProvenanceUnembedded] proves over the tree that is
// actually published.
func (r *run) capturedProvenance(ctx context.Context, pass1 relocate.FileSet, records []provenanceRecord) (map[string]bool, error) {
	candidates := make(map[string]bool, len(records))
	present := destinationPaths(pass1)
	for _, record := range records {
		destination := r.destinationOf(record.source)
		candidates[destination] = true
		present = append(present, destination)
	}
	slices.Sort(present)

	captured := make(map[string]bool)
	for _, file := range pass1.Files {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("plan provenance: %w", err)
		}
		if !r.isGoSource(file.Source) {
			continue
		}
		embeds, err := rewrite.VerifyEmbeds(ctx,
			rewrite.File{Path: file.Path, Contents: r.finalContents(file.Path)}, present)
		if err != nil {
			return nil, rewritePolicy(err)
		}
		for _, embed := range embeds {
			for _, match := range embed.Matches {
				if candidates[match] {
					captured[match] = true
				}
			}
		}
	}
	return captured, nil
}

// destinationOf maps an upstream relative path onto its module relative one,
// which is the mapping relocation performs.
func (r *run) destinationOf(source string) string {
	return path.Join(r.cfg.Destination.InternalPrefix, source)
}

// centralProvenancePath renders the upstream relative path a displaced record
// is relocated from.
//
// The package directory is flattened into one file name, and a digest of the
// original is appended so two directories that flatten to the same text still
// get separate records. The digest is over the package directory alone, so it
// depends on the profile rather than on the machine.
func centralProvenancePath(pkg string) string {
	return provenanceDirName + "/" + escapePackageDir(pkg) + "-" + shortDigest(pkg) + ".txt"
}

// escapePackageDir renders a package directory as one path element.
func escapePackageDir(pkg string) string {
	return strings.ReplaceAll(pkg, "/", "_")
}

// render builds one package's provenance record.
//
// The record is returned rather than its rendering, because the same values
// become both the committed text and the evidence the plan hands back. Rendering
// it twice from one structure is what keeps the two from ever disagreeing.
func (r *run) render(record provenanceRecord, pass1 relocate.FileSet) (*rewrite.PackageProvenance, error) {
	rendered := rewrite.NewPackageProvenance(r.destinationOf(record.pkg), record.pkg, r.baseRewriteOptions())
	for _, destination := range record.files {
		file, ok := pass1.Lookup(destination)
		if !ok {
			return nil, fmt.Errorf("plan provenance: %q is attributed to package %q but not in the relocated set",
				destination, record.pkg)
		}
		rendered.AddFile(rewrite.File{
			Path:       file.Path,
			SourcePath: file.Source,
			Generated:  file.Generated,
		}, r.results[destination])
	}
	for _, entry := range r.cfg.Prune.Files {
		if path.Dir(entry) == record.pkg {
			rendered.AddPruned(entry)
		}
	}
	rendered.AddPatches(r.applied...)
	return rendered, nil
}

// buildFinalTree relocates the rewritten bytes and the generated records
// together.
//
// The second pass carries everything that was produced after the first through
// the same validation the first performed, so no file reaches a tree unchecked.
func (r *run) buildFinalTree(ctx context.Context, pass1 relocate.FileSet, records []relocate.PlanFile) error {
	files := make([]relocate.PlanFile, 0, len(pass1.Files)+len(records))
	for _, file := range pass1.Files {
		files = append(files, relocate.PlanFile{
			Path:    file.Source,
			Package: file.SourcePackage,
			// The mode travels around the rewriting step rather than through it.
			// A transformation changes bytes; whether a file is executable is a
			// property of the upstream file, and nothing here may alter it.
			Mode:      file.Mode,
			Contents:  r.finalContents(file.Path),
			Generated: file.Generated,
		})
	}
	files = append(files, records...)

	pass2, err := relocate.Build(ctx, relocate.Plan{Files: files}, r.relocateOptions())
	if err != nil {
		return contentPolicy("plan relocate", err)
	}
	r.tree = pass2
	return nil
}

// setEngine records what produced the plan.
//
// It runs before any phase, so a report produced for a failure still identifies
// the engine and the profile that refused. Nothing in it depends on a phase
// having run.
func (r *run) setEngine() error {
	profileBytes, err := r.cfg.ProfileBytes()
	if err != nil {
		return fmt.Errorf("plan report: %w", err)
	}
	r.report.Engine = EngineReport{
		Version:     buildinfo.Version,
		Toolchain:   r.cfg.Determinism.Toolchain,
		ProfileHash: profileHash(profileBytes, buildinfo.Version),
	}
	return nil
}

// profileHash binds an epoch to both output-affecting configuration and the
// released engine that interpreted it. Length framing keeps the concatenation
// unambiguous if either input later contains the separator text.
func profileHash(profile []byte, engineVersion string) string {
	var identity bytes.Buffer
	fmt.Fprintf(&identity, "soapbox-profile-v1\nprofile %d\n", len(profile))
	identity.Write(profile)
	fmt.Fprintf(&identity, "\nengine %d\n%s", len(engineVersion), engineVersion)
	return "sha256:" + contentDigest(identity.Bytes())
}

// checkToolchain refuses a Go toolchain other than the one the profile pins.
//
// The plan reformats every relocated Go file with the toolchain it is running
// under and reports the module as gofmt clean under the pinned one. gofmt's
// output has changed between Go releases, so a run under a different toolchain
// is making a claim it cannot support: the module it measured would be
// reformatted by the toolchain the profile names. The check is against
// [runtime.Version] rather than against a go binary on the path, because the
// formatter that runs is the one compiled into this process.
func (r *run) checkToolchain() error {
	pinned := r.cfg.Determinism.Toolchain
	running := runtime.Version()
	if running == pinned {
		return nil
	}
	// A toolchain built from source reports a devel string carrying a commit
	// rather than a release name, so it can never equal the pin. Saying which
	// of the two mismatches this is saves the reader working it out from a
	// version string they did not expect to see.
	if strings.HasPrefix(running, "devel") {
		return policyf("plan toolchain",
			"the profile pins %s for deterministic formatting and this is a development toolchain (%s), whose gofmt output is not the pinned one",
			pinned, running)
	}
	return policyf("plan toolchain",
		"the profile pins %s for deterministic formatting and this plan is running under %s", pinned, running)
}

// compareGolden compares the settled closure against the record the profile
// pins.
//
// The golden is the closure a maintainer reviewed. Limits alone cannot replace
// it: they notice a closure that grew past a number, while a package swapped
// for another of the same size passes every limit and is exactly the change a
// review exists to catch. Only the exact shape is compared, because the observed
// half is measurement that changes with upstream without the profile changing at
// all.
//
// A golden the profile names but the directory does not hold is a notice rather
// than a failure, because the first run that establishes a closure has nothing
// to compare against; -strict refuses it, which is how CI insists on one. A
// golden that exists and cannot be read or decoded is a failure, because it
// names a file the maintainer meant to be authoritative. Nothing here writes or
// updates a golden: a gate that repairs itself gates nothing.
func (r *run) compareGolden(ctx context.Context) error {
	relative := r.cfg.Closure.Golden
	if relative == "" {
		return nil
	}
	r.report.Closure.Golden.Path = relative

	resolved, err := config.SafeJoin(ctx, r.opts.ProfileDir, relative)
	if err != nil {
		return fmt.Errorf("plan golden: %w", err)
	}
	// The path is the profile's own, resolved through SafeJoin, which refuses
	// anything that leaves the profile directory before and after symbolic link
	// resolution. That is the containment this read needs.
	contents, err := os.ReadFile(resolved) //nolint:gosec // resolved by config.SafeJoin below the profile directory
	switch {
	case errors.Is(err, os.ErrNotExist):
		r.report.Closure.Golden.Status = GoldenAbsent
		r.notices = append(r.notices, "the profile names the closure golden "+relative+", which is not in the profile directory")
		return nil
	case err != nil:
		return fmt.Errorf("plan golden: read %s: %w", relative, err)
	}

	var golden closure.ClosureReport
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&golden); err != nil {
		return fmt.Errorf("plan golden: decode %s: %w", relative, err)
	}

	differences := compareExact(golden.Exact, r.report.Closure.Report.Exact)
	if len(differences) == 0 {
		r.report.Closure.Golden.Status = GoldenMatch
		return nil
	}
	r.report.Closure.Golden.Status = GoldenDiff
	r.report.Closure.Golden.Differences = differences
	return policyf("plan golden", "the closure no longer matches %s:\n  %s", relative, strings.Join(differences, "\n  "))
}

// compareExact renders the fields of two exact closure shapes that disagree.
//
// The comparison is literal and per field. A single "they differ" would leave
// the reader diffing two JSON documents to find out what changed, and the whole
// value of the gate is that the change is named.
func compareExact(golden, current closure.ExactShape) []string {
	var differences []string
	if golden.ImportPrefix != current.ImportPrefix {
		differences = append(differences, fmt.Sprintf("importPrefix is %q, golden has %q", current.ImportPrefix, golden.ImportPrefix))
	}
	for _, field := range []struct {
		name   string
		golden []string
		got    []string
	}{
		{"roots", golden.Roots, current.Roots},
		{"packages", golden.Packages, current.Packages},
		{"files", golden.Files, current.Files},
		{"externalPackages", golden.ExternalPackages, current.ExternalPackages},
		{"standardPackages", golden.StandardPackages, current.StandardPackages},
		{"prunedFiles", golden.PrunedFiles, current.PrunedFiles},
		{"deniedImports", golden.DeniedImports, current.DeniedImports},
	} {
		for _, added := range missingFrom(field.got, field.golden) {
			differences = append(differences, field.name+" gained "+added)
		}
		for _, removed := range missingFrom(field.golden, field.got) {
			differences = append(differences, field.name+" lost "+removed)
		}
	}
	slices.Sort(differences)
	return differences
}

// missingFrom reports the members of one list the other does not hold, sorted.
func missingFrom(values, other []string) []string {
	var out []string
	for _, value := range values {
		if !slices.Contains(other, value) {
			out = append(out, value)
		}
	}
	slices.Sort(out)
	return out
}

// finish assembles the report, applies the gates, and writes the tree when
// asked.
//
// The order is the contract. Every gate runs before anything is written, so a
// plan that is going to refuse refuses without leaving an output tree behind:
// -strict -materialize on a profile with notices has to be immediately
// retryable, and it would not be if the refusal came after a tree existed at a
// destination that must not exist.
func (r *run) finish(ctx context.Context) (*Result, error) {
	r.report.Worktree.SparsePatterns = slices.Clone(r.patterns)
	r.report.Worktree.WidenRounds = len(r.widened)
	r.report.Worktree.WidenedPackages = sorted(r.widened)

	r.summarizeRewrites()
	if err := r.summarizeTree(); err != nil {
		return nil, err
	}

	// A cache that moved is checked before the gates rather than after, because
	// the answer decides whether this run may write anything at all.
	cacheErr := r.assertCacheUnchanged(ctx)
	goldenErr := r.compareGolden(ctx)
	if goldenErr != nil && !isPolicy(goldenErr) {
		// An unreadable or malformed golden says nothing about the closure, so
		// it is a runtime failure and produces no report to read it from.
		return nil, goldenErr
	}

	r.report.Notices = sorted(r.notices)
	normalize(&r.report)

	switch {
	case cacheErr != nil:
		return r.result(), cacheErr
	case goldenErr != nil:
		return r.result(), goldenErr
	case r.opts.Strict && len(r.report.Notices) > 0:
		return r.result(), policyf("plan strict", "%d notices under -strict:\n  %s",
			len(r.report.Notices), strings.Join(r.report.Notices, "\n  "))
	}

	if r.opts.Materialize {
		if err := relocate.Materialize(ctx, r.opts.OutputRoot, r.tree); err != nil {
			// The result still goes back. The plan itself succeeded, the tree it
			// computed is in hand, and the report is what tells an operator which
			// cache and work state the failed write leaves behind.
			return r.result(), fmt.Errorf("plan materialize: %w", err)
		}
		r.report.Output.Materialized = true
	}
	return r.result(), nil
}

// result renders the run's current state as a completed plan.
//
// The evidence is copied on the way out. A caller holds the only reference to a
// result and has every reason to sort, filter, and annotate what it finds there,
// while the run keeps reading its own copy to check the invariants that follow.
// Handing out the run's slices would make one of those two harmless activities
// silently corrupt the other.
func (r *run) result() *Result {
	return &Result{
		Report:     r.report,
		Files:      r.tree,
		Provenance: clonePackageProvenance(r.records),
		Paths:      r.paths,
	}
}

// clonePackageProvenance deep copies the per-package records.
//
// Every level that a caller could reach and write through is copied: the slice
// of pointers, each record, its file list, each file's change list, and the
// pruned and patch lists. What remains shared is immutable, which is why the
// copy stops there rather than continuing into strings.
func clonePackageProvenance(records []*rewrite.PackageProvenance) []*rewrite.PackageProvenance {
	if len(records) == 0 {
		return nil
	}
	out := make([]*rewrite.PackageProvenance, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		copied := *record
		copied.Files = slices.Clone(record.Files)
		for i, file := range copied.Files {
			copied.Files[i].Changes = slices.Clone(file.Changes)
		}
		copied.Pruned = slices.Clone(record.Pruned)
		copied.Patches = slices.Clone(record.Patches)
		out = append(out, &copied)
	}
	return out
}

// failedResult records why a plan refused and renders the partial report.
//
// A refusal is the case a report is most needed for, so it is produced rather
// than suppressed: a patch conflict, a closure past a limit, or a cache that
// moved are all findings CI has to keep, and a run that only printed them to
// stderr would leave nothing to attach.
func (r *run) failedResult(err error) *Result {
	r.report.Failure = r.newFailureReport(err)
	r.report.Notices = sorted(r.notices)
	normalize(&r.report)
	return r.result()
}

// newFailureReport renders one failure for the report.
func (r *run) newFailureReport(err error) *FailureReport {
	if err == nil {
		return nil
	}
	stage := policyStage(err)
	if stage == "" {
		stage = "plan"
	}
	failure := &FailureReport{Stage: stage, Message: r.scrub(err.Error())}
	var conflict *patchset.ConflictError
	if errors.As(err, &conflict) {
		failure.Patch = newPatchFailure(conflict)
	}
	return failure
}

// newPatchFailure renders a patch conflict for the report.
//
// The whole of the error is carried across, including the captured diff. A
// maintainer's next step after a conflict is to rewrite the patch against the
// new upstream, and the diff with its conflict markers is what they rewrite it
// from; a report that named the patch and dropped the evidence would still
// require the pipeline to be rerun.
func newPatchFailure(conflict *patchset.ConflictError) *PatchFailure {
	status := make([]string, 0, len(conflict.Status))
	for _, entry := range conflict.Status {
		// The porcelain code and the path are joined the way git prints them,
		// so a reader who knows the two character codes reads the report
		// without a legend.
		status = append(status, entry.Code+" "+entry.Path)
	}
	return &PatchFailure{
		SourceRef:       conflict.SourceRef,
		SourceSHA:       conflict.SourceSHA,
		PatchID:         conflict.PatchID,
		PatchIndex:      conflict.PatchIndex,
		PatchCount:      conflict.PatchCount,
		Stage:           string(conflict.Stage),
		ConflictedPaths: nonNil(slices.Clone(conflict.ConflictedPaths)),
		Status:          status,
		Diff:            conflict.Diff,
	}
}

// scrub replaces every absolute directory this run was given with a stable
// placeholder.
//
// A failure message is the one part of the report assembled from text a lower
// package wrote, and those packages name the file they were working on. The
// report is compared byte for byte between two runs over different layouts and
// is attached to CI artifacts, so a machine's paths may not survive into it.
// Replacement is longest first, because the documented defaults nest the work
// root below the cache and the output below the work root.
func (r *run) scrub(message string) string {
	replacements := []struct {
		dir         string
		placeholder string
	}{
		{r.paths.Worktree, "<worktree>"},
		{r.paths.Output, "<output>"},
		{r.paths.Work, "<work>"},
		{r.paths.Cache, "<cache>"},
		{r.opts.CacheRoot, "<cache-root>"},
		{r.opts.ProfileDir, "<profile>"},
		{r.opts.SourceRemote, "<source>"},
	}
	slices.SortFunc(replacements, func(a, b struct {
		dir         string
		placeholder string
	},
	) int {
		return len(b.dir) - len(a.dir)
	})
	for _, replacement := range replacements {
		if replacement.dir != "" {
			message = strings.ReplaceAll(message, replacement.dir, replacement.placeholder)
		}
	}
	return message
}

// assertCacheUnchanged proves the plan moved nothing in the shared cache.
//
// The scratch anchor is the only commit a plan makes, and it is made on a
// detached HEAD in a linked work tree, so no ref may differ. Comparing rather
// than trusting is the point: the cache is reused across runs and is what later
// phases publish from, so a plan that silently moved a ref would corrupt every
// run after it.
//
// The comparison runs on a detached context, because one of the ways it is
// reached is a cancelled plan, and an invariant that is only checked when
// nothing went wrong is not an invariant. The answer is memoised so the failure
// paths, which check again on the way out, cannot report a different one from
// the success path.
func (r *run) assertCacheUnchanged(ctx context.Context) error {
	if r.cacheChecked {
		return r.cacheErr
	}
	if r.cache == nil || r.cacheRefs == nil {
		// The snapshot is taken at the end of source acquisition, so a run that
		// failed before then never established what it would be compared to.
		return nil
	}
	r.cacheChecked = true

	detached, cancel := detachedContext(ctx)
	defer cancel()
	after, err := r.cache.Git().ListRefs(detached)
	if err != nil {
		r.cacheErr = fmt.Errorf("plan report: %w", err)
		return r.cacheErr
	}
	before, current := renderRefs(r.cacheRefs), renderRefs(after)
	if slices.Equal(before, current) {
		return nil
	}
	r.report.Worktree.CacheRefsMoved = true
	r.cacheErr = fmt.Errorf("plan report: the source cache changed while the plan ran: %s became %s",
		strings.Join(before, " "), strings.Join(current, " "))
	return r.cacheErr
}

// renderRefs projects a ref list onto the comparable form, sorted so the
// comparison never depends on the order git listed them in.
func renderRefs(refs []gitcli.Ref) []string {
	rendered := make([]string, 0, len(refs))
	for _, ref := range refs {
		rendered = append(rendered, ref.Name+"="+ref.Target)
	}
	slices.Sort(rendered)
	return rendered
}

// summarizeRewrites renders the per-file transformation record.
func (r *run) summarizeRewrites() {
	for destination, result := range r.results {
		if !result.Changed() {
			continue
		}
		file := RewrittenFile{Path: destination}
		for _, change := range result.Changes {
			file.Changes = append(file.Changes, change.String())
			switch change.Kind {
			case rewrite.ChangeNotice:
				file.NoticeInserted = true
			case rewrite.ChangeMarkerRemoval, rewrite.ChangeCommentRemoval:
				r.report.Rewrite.DirectiveRemovals = append(r.report.Rewrite.DirectiveRemovals, change.String())
			}
		}
		slices.Sort(file.Changes)
		r.report.Rewrite.Files = append(r.report.Rewrite.Files, file)
	}
	slices.SortFunc(r.report.Rewrite.Files, func(a, b RewrittenFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	slices.Sort(r.report.Rewrite.DirectiveRemovals)
	slices.Sort(r.report.Rewrite.Unparsed)
	slices.Sort(r.report.Rewrite.Unformatted)
	slices.SortFunc(r.report.Rewrite.Embeds, func(a, b EmbedReport) int {
		if order := strings.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return a.Line - b.Line
	})
}

// summarizeTree renders the relocation and output sections and hashes the tree.
//
// A destination the package index names and the file set does not hold is an
// engine fault rather than a finding, and it stops the run: the manifest hash is
// what two plans compare to decide they produced the same module, and a hash
// that silently covered fewer files than the tree holds would make two different
// modules agree.
func (r *run) summarizeTree() error {
	r.report.Relocation.InternalPrefix = r.cfg.Destination.InternalPrefix
	entries := make([]manifestEntry, 0, len(r.tree.Files))

	for _, pkg := range r.tree.Packages {
		relocated := RelocatedPackage{SourcePackage: pkg.Source, Package: pkg.Path}
		for _, destination := range pkg.Files {
			file, ok := r.tree.Lookup(destination)
			if !ok {
				return fmt.Errorf("plan report: %q is listed in package %q but not in the relocated set",
					destination, pkg.Path)
			}
			digest := contentDigest(file.Contents)
			generated := r.provenance[file.Path]
			source := file.Source
			if generated {
				// Relocation maps a provenance record from a notional upstream
				// path so it lands beside the package it describes. Reporting
				// that path as its origin would claim upstream ships a file it
				// has never heard of.
				source = ""
				r.report.Output.ProvenanceFiles = append(r.report.Output.ProvenanceFiles, file.Path)
			}
			relocated.Files = append(relocated.Files, RelocatedFile{
				Source:      source,
				Destination: file.Path,
				Mode:        file.Mode.String(),
				Generated:   file.Generated,
				SHA256:      digest,
			})
			entries = append(entries, manifestEntry{path: file.Path, mode: file.Mode.String(), digest: digest})
		}
		r.report.Relocation.Packages = append(r.report.Relocation.Packages, relocated)
	}
	if got, want := len(entries), len(r.tree.Files); got != want {
		return fmt.Errorf("plan report: the manifest covers %d files and the tree holds %d", got, want)
	}
	slices.Sort(r.report.Output.ProvenanceFiles)

	r.report.Output.Module = r.cfg.Destination.Module
	r.report.Output.Files = len(r.tree.Files)
	r.report.Output.Packages = len(r.closureResult.Packages)
	r.report.Output.ManifestHash = manifestHash(entries)
	return nil
}

// sorted returns a sorted, deduplicated, non-nil copy so the encoded report
// never depends on discovery order and never renders null.
func sorted(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		return []string{}
	}
	return out
}

// normalize replaces every nil list in the report with an empty one.
//
// A report that flips between null and [] when a profile stops pruning is noise
// in a diff a reviewer has to read past, and a consumer that has to handle both
// spellings of "nothing" will eventually handle only one. Nested lists are
// covered as carefully as top level ones, because a consumer meets them the same
// way.
func normalize(report *Report) {
	report.Source.RefKind = strings.TrimSpace(report.Source.RefKind)
	report.Worktree.SparsePatterns = nonNil(report.Worktree.SparsePatterns)
	report.Worktree.WidenedPackages = nonNil(report.Worktree.WidenedPackages)
	report.Worktree.ScratchAnchor.StagedDeletions = nonNil(report.Worktree.ScratchAnchor.StagedDeletions)
	report.Patches.Selected = nonNil(report.Patches.Selected)
	report.Patches.Applied = nonNil(report.Patches.Applied)
	report.Patches.Reassert = nonNilSlice(report.Patches.Reassert)
	for i := range report.Patches.Reassert {
		report.Patches.Reassert[i].Files = nonNil(report.Patches.Reassert[i].Files)
	}
	report.Closure.RemovedFiles = nonNil(report.Closure.RemovedFiles)
	report.Closure.Golden.Differences = nonNil(report.Closure.Golden.Differences)
	normalizeClosure(&report.Closure.Report.Exact)
	report.Relocation.Packages = nonNilSlice(report.Relocation.Packages)
	for i := range report.Relocation.Packages {
		report.Relocation.Packages[i].Files = nonNilSlice(report.Relocation.Packages[i].Files)
	}
	report.Rewrite.Files = nonNilSlice(report.Rewrite.Files)
	for i := range report.Rewrite.Files {
		report.Rewrite.Files[i].Changes = nonNil(report.Rewrite.Files[i].Changes)
	}
	report.Rewrite.DirectiveRemovals = nonNil(report.Rewrite.DirectiveRemovals)
	report.Rewrite.Embeds = nonNilSlice(report.Rewrite.Embeds)
	for i := range report.Rewrite.Embeds {
		report.Rewrite.Embeds[i].Patterns = nonNil(report.Rewrite.Embeds[i].Patterns)
		report.Rewrite.Embeds[i].Matches = nonNil(report.Rewrite.Embeds[i].Matches)
	}
	report.Rewrite.Unparsed = nonNil(report.Rewrite.Unparsed)
	report.Rewrite.Unformatted = nonNil(report.Rewrite.Unformatted)
	report.Output.ProvenanceFiles = nonNil(report.Output.ProvenanceFiles)
	if report.Failure != nil && report.Failure.Patch != nil {
		report.Failure.Patch.ConflictedPaths = nonNil(report.Failure.Patch.ConflictedPaths)
		report.Failure.Patch.Status = nonNil(report.Failure.Patch.Status)
	}
	report.Notices = nonNil(report.Notices)
}

// nonNil replaces a nil string list with an empty one.
func nonNil(values []string) []string { return nonNilSlice(values) }

// normalizeClosure fills in the closure's own lists.
//
// The closure builds them non-nil, so a completed plan never needs this. A plan
// that refused before the closure settled carries the zero value instead, and
// its report is exactly the one a reviewer reads: the section has to spell
// "nothing" the same way there as everywhere else.
func normalizeClosure(exact *closure.ExactShape) {
	exact.Roots = nonNil(exact.Roots)
	exact.Packages = nonNil(exact.Packages)
	exact.Files = nonNil(exact.Files)
	exact.ExternalPackages = nonNil(exact.ExternalPackages)
	exact.StandardPackages = nonNil(exact.StandardPackages)
	exact.PrunedFiles = nonNil(exact.PrunedFiles)
	exact.DeniedImports = nonNil(exact.DeniedImports)
}

// nonNilSlice replaces a nil list of any element type with an empty one.
func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

// shortDigest renders the leading bytes of a value's digest, which is enough to
// separate names that would otherwise collide.
func shortDigest(value string) string {
	const width = 8
	return contentDigest([]byte(value))[:width]
}
