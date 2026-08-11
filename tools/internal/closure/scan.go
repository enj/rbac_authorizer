package closure

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// CopyKind classifies why a file is in the copy plan.
type CopyKind string

// Copy kinds. The set covers what a portable build of a relocated package
// needs: Go source, the native sources and objects the go tool hands to the C
// and assembly toolchains, and the data files that Go source references without
// importing.
const (
	// KindGo is a non-test Go source file.
	KindGo CopyKind = "go"
	// KindGoTest is a _test.go file, present only when tests are included.
	KindGoTest CopyKind = "gotest"
	// KindNative is a cgo, C, C++, Objective-C, Fortran, or SWIG source file.
	KindNative CopyKind = "native"
	// KindHeader is a C or C++ header included by native or cgo sources.
	KindHeader CopyKind = "header"
	// KindAssembly is a Go assembly or preprocessed assembly source file.
	KindAssembly CopyKind = "asm"
	// KindObject is a prebuilt .syso object linked into the package.
	KindObject CopyKind = "object"
	// KindEmbed is a file selected by a go:embed directive.
	KindEmbed CopyKind = "embed"
	// KindAsset is a file selected by a configured asset glob.
	KindAsset CopyKind = "asset"
)

// companionKinds maps a file extension to the copy kind the go tool would give
// it. Extensions are compared case sensitively, exactly as the go tool compares
// them, which is why .s and .S are separate entries.
//
// The set is the exact set go/build recognises, which is the list its
// fileListForExt switch dispatches on. Anything outside it is not a build input:
// .mm is the instructive absence, because Objective-C++ looks like it belongs
// beside .m and .cc but the go tool has never compiled it, so carrying it would
// copy a file no build reads while implying the generated module supports it.
var companionKinds = map[string]CopyKind{
	".c":       KindNative,
	".cc":      KindNative,
	".cpp":     KindNative,
	".cxx":     KindNative,
	".m":       KindNative,
	".f":       KindNative,
	".F":       KindNative,
	".for":     KindNative,
	".f90":     KindNative,
	".swig":    KindNative,
	".swigcxx": KindNative,
	".h":       KindHeader,
	".hh":      KindHeader,
	".hpp":     KindHeader,
	".hxx":     KindHeader,
	".s":       KindAssembly,
	".S":       KindAssembly,
	".sx":      KindAssembly,
	".syso":    KindObject,
}

// scanMode carries the per-pass decisions that change how a package is read.
//
// The two passes of a build differ in what they are for, not only in what they
// check. The pre-prune pass measures what upstream contained and runs over
// packages pruning is about to remove; the post-prune pass decides what is
// copied and judges the result.
type scanMode struct {
	// includeTests adds _test.go files to the package and follows their imports.
	includeTests bool
	// strict requires every file to parse and every file to agree about the
	// package name. The post-prune pass is always strict.
	strict bool
	// prunable holds the files this build is about to remove. A file listed here
	// is allowed to be malformed while the tolerant pass measures the tree,
	// because removing it is the remedy the profile already expresses and the
	// measurement must not veto the contract it is measuring.
	prunable map[string]bool
}

// tolerates reports whether a failure reading one file may be absorbed.
//
// Tolerance is narrow on both axes. Only the pre-prune pass tolerates anything,
// only a file the profile has scheduled for removal is eligible, and only a
// failure caused by that file's own content qualifies: an unsafe symbolic link
// or a filesystem error stops the run no matter which file it names.
func (m scanMode) tolerates(file string, err error) bool {
	return !m.strict && m.prunable[file] && isContentError(err)
}

// pkgScan is one materialized package as found on disk.
type pkgScan struct {
	// dir is the package directory relative to the materialized root.
	dir string
	// importPath is the package's path within the source module.
	importPath string
	// name is the Go package name declared by its files.
	name string
	// goFiles are the package's Go files in sorted path order.
	goFiles []goFile
	// companions are the package's non-Go build inputs in sorted path order.
	companions []planFile
}

// goFile is one parsed Go source file.
type goFile struct {
	path    string
	pkgName string
	test    bool
	imports []string
	embeds  []string
	lines   int
	mode    fs.FileMode
	// malformed marks a file the tolerant pre-prune pass could not parse. It is
	// recorded rather than dropped so that the prune entry naming it can still be
	// located and removed, and so that the pre-prune line count stays honest. It
	// declares no package name and contributes no imports, because nothing that
	// did not parse can be trusted to state either.
	malformed bool
}

// planFile is one non-Go file destined for the copy plan.
type planFile struct {
	path string
	kind CopyKind
	mode fs.FileMode
}

// nonTestLines counts the lines of the package's non-test Go files.
func (p *pkgScan) nonTestLines() int {
	total := 0
	for _, f := range p.goFiles {
		if !f.test {
			total += f.lines
		}
	}
	return total
}

// imports returns every import of the package, sorted and deduplicated.
func (p *pkgScan) imports() []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 16)
	for _, f := range p.goFiles {
		for _, imp := range f.imports {
			if !seen[imp] {
				seen[imp] = true
				out = append(out, imp)
			}
		}
	}
	slices.Sort(out)
	return out
}

// scanPackage reads one package directory and parses its Go files.
//
// Only the directory's own files are read. A subdirectory is never descended
// into, because a sibling subpackage is part of the closure exactly when it is
// imported or explicitly configured, never because it happens to sit below a
// package that is.
//
// Every non-test Go file is parsed regardless of its build constraints and
// regardless of any GOOS or GOARCH filename suffix. The result is the portable
// union of every platform's file set: the generated module has to build on
// every platform the upstream package builds on, so a linux-only import is as
// much a part of the closure as an unconstrained one.
//
// strict controls whether the package name must be consistent and whether every
// file must parse. The pre-prune pass runs unstrict because a directory can hold
// a stray "//go:build ignore" generator declaring package main, or a file that
// upstream left unparsable, and pruning that file is precisely the remedy; the
// post-prune pass runs strict, so a real inconsistency still fails.
func scanPackage(ctx context.Context, w *worktree, dir, importPath string, mode scanMode) (*pkgScan, error) {
	// The directory is confirmed before it is listed so that an import naming
	// something that is not a package reads as a missing package rather than as
	// a platform specific listing error.
	switch isDir, err := w.isDir(ctx, dir); {
	case isNotFound(err), err == nil && !isDir:
		return nil, &PackageError{Dir: displayPath(dir), ImportPath: importPath, Err: ErrPackageMissing}
	case err != nil:
		return nil, err
	}
	entries, err := w.readDir(ctx, dir)
	if err != nil {
		return nil, err
	}

	pkg := &pkgScan{dir: dir, importPath: importPath}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("scan package %s: %w", displayPath(dir), err)
		}
		name := entry.Name()
		// The go tool ignores names beginning with a dot or an underscore, so
		// the closure must ignore them too or it would copy files that no build
		// ever compiles.
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		rel := joinPath(dir, name)
		// Any link is refused, not only one that escapes. os.Root already
		// guarantees containment; what it cannot do is make a link meaningful
		// to a copy plan, and a materialized upstream package directory has no
		// legitimate reason to hold one.
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, &FileError{File: rel, Err: ErrUnsafeSymlink}
		}
		if entry.IsDir() {
			continue
		}
		kind, wanted := classifyFile(name, mode.includeTests)
		if !wanted {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", rel, err)
		}

		if kind != KindGo && kind != KindGoTest {
			pkg.companions = append(pkg.companions, planFile{path: rel, kind: kind, mode: info.Mode().Perm()})
			continue
		}
		parsed, err := parseGoFile(ctx, w, fset, rel, kind == KindGoTest, info.Mode().Perm())
		if err != nil {
			if parsed == nil || !mode.tolerates(rel, err) {
				return nil, err
			}
		}
		pkg.goFiles = append(pkg.goFiles, *parsed)
	}

	if len(pkg.goFiles) == 0 {
		return nil, &PackageError{Dir: displayPath(dir), ImportPath: importPath, Err: ErrPackageMissing}
	}
	if err := pkg.resolveName(mode.strict); err != nil {
		return nil, err
	}
	return pkg, nil
}

// resolveName picks the package's Go name and, when strict, proves that every
// file agrees on it.
//
// The name is the one most of the package's files declare, not the one that
// happens to sort first. The difference is what the error names: a directory
// holding a stray "//go:build ignore" generator has one file declaring main and
// several declaring the real name, and taking the first sorted file as the
// authority would report the disagreement against whichever ordinary file
// followed it. That file is innocent and pruning it would be wrong, so the
// majority decides and every outlier is named, since the outliers are the prune
// candidates an operator is looking for.
func (p *pkgScan) resolveName(strict bool) error {
	p.name = majorityName(p.goFiles)
	if !strict {
		return nil
	}

	var conflicts []string
	for _, f := range p.goFiles {
		switch {
		case f.pkgName == p.name:
		case f.test && f.pkgName == p.name+"_test":
		default:
			conflicts = append(conflicts, fmt.Sprintf("%s declares %q", f.path, f.pkgName))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return &PackageError{
		Dir:        displayPath(p.dir),
		ImportPath: p.importPath,
		Err: fmt.Errorf("%w: files disagree about the package name: the package is %q, but %s",
			ErrPackageMalformed, p.name, strings.Join(conflicts, "; ")),
	}
}

// majorityName reports the package name the most files declare.
//
// Non-test files decide it. A directory that holds only test files still has a
// name, so the external test package suffix is stripped and those files vote
// instead. Ties break on the lexically smaller name so that a package split
// evenly between two names always fails the same way, and a file that did not
// parse casts no vote because nothing it appears to declare can be trusted.
func majorityName(files []goFile) string {
	votes := make(map[string]int, 2)
	for _, f := range files {
		if f.malformed || f.test {
			continue
		}
		votes[f.pkgName]++
	}
	if len(votes) == 0 {
		for _, f := range files {
			if !f.malformed {
				votes[strings.TrimSuffix(f.pkgName, "_test")]++
			}
		}
	}

	best := ""
	for _, name := range sortedKeys(votes) {
		if best == "" || votes[name] > votes[best] {
			best = name
		}
	}
	return best
}

// parseGoFile reads and parses one Go file.
//
// Comments are retained because go:embed directives live in them and decide
// which non-Go files the copy plan must carry.
//
// A file that reads but does not parse returns both a record and an error. The
// record states only what remains true of a file nobody can interpret, its path,
// its mode, and its length, and exists for the single caller allowed to tolerate
// the failure: the pre-prune pass measuring a file the profile is about to
// remove. A file that cannot be read at all returns no record, because a
// filesystem or symbolic link failure is never tolerated.
func parseGoFile(ctx context.Context, w *worktree, fset *token.FileSet, rel string, test bool, mode fs.FileMode) (*goFile, error) {
	src, err := w.readFile(ctx, rel)
	if err != nil {
		return nil, err
	}
	unparsed := &goFile{path: rel, test: test, lines: countLines(src), mode: mode, malformed: true}

	file, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
	if err != nil {
		return unparsed, &FileError{File: rel, Err: fmt.Errorf("%w: %w", ErrPackageMalformed, err)}
	}

	imports := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		// An import alias, a blank import, and a dot import all pull the same
		// package into the build, so only the literal path matters here.
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return unparsed, &FileError{File: rel, Err: fmt.Errorf("%w: malformed import %s", ErrPackageMalformed, spec.Path.Value)}
		}
		imports = append(imports, value)
	}
	slices.Sort(imports)
	imports = slices.Compact(imports)

	embeds, err := embedPatterns(file)
	if err != nil {
		return unparsed, &FileError{File: rel, Err: err}
	}

	return &goFile{
		path:    rel,
		pkgName: file.Name.Name,
		test:    test,
		imports: imports,
		embeds:  embeds,
		lines:   countLines(src),
		mode:    mode,
	}, nil
}

// classifyFile reports the copy kind of a directory entry and whether the
// closure wants it at all.
//
// Everything unrecognised is deliberately dropped: OWNERS, BUILD, BUILD.bazel,
// .import-restrictions, and README files are repository metadata that the
// generated module must not inherit, and no build input has their names.
func classifyFile(name string, includeTests bool) (CopyKind, bool) {
	if strings.HasSuffix(name, ".go") {
		if strings.HasSuffix(name, "_test.go") {
			return KindGoTest, includeTests
		}
		return KindGo, true
	}
	kind, ok := companionKinds[path.Ext(name)]
	return kind, ok
}

// embedPatterns extracts every go:embed pattern declared in the file.
//
// Only directives attached to a var declaration are honoured, which is the rule
// the go tool applies. Scanning every comment instead would let a commented out
// directive, or a directive quoted in documentation, demand files that no build
// ever reads and turn an unrelated upstream edit into a spurious failure.
func embedPatterns(file *ast.File) ([]string, error) {
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		groups := []*ast.CommentGroup{gen.Doc}
		for _, spec := range gen.Specs {
			if value, ok := spec.(*ast.ValueSpec); ok {
				groups = append(groups, value.Doc)
			}
		}
		for _, group := range groups {
			if group == nil {
				continue
			}
			for _, comment := range group.List {
				rest, found := strings.CutPrefix(comment.Text, "//go:embed")
				if !found || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
					continue
				}
				patterns, err := splitEmbedPatterns(strings.TrimSpace(rest))
				if err != nil {
					return nil, err
				}
				out = append(out, patterns...)
			}
		}
	}
	return out, nil
}

// splitEmbedPatterns splits a go:embed directive body into patterns. Patterns
// are space separated and may be bare, interpreted quoted, or raw quoted.
func splitEmbedPatterns(line string) ([]string, error) {
	var out []string
	for {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			return out, nil
		}
		var pattern string
		switch line[0] {
		case '`':
			end := strings.IndexByte(line[1:], '`')
			if end < 0 {
				return nil, fmt.Errorf("%w: unterminated raw quoted go:embed pattern", ErrPatternMalformed)
			}
			pattern, line = line[1:1+end], line[2+end:]
		case '"':
			end := 1
			for ; end < len(line); end++ {
				if line[end] == '\\' {
					end++
					continue
				}
				if line[end] == '"' {
					break
				}
			}
			if end >= len(line) {
				return nil, fmt.Errorf("%w: unterminated quoted go:embed pattern", ErrPatternMalformed)
			}
			unquoted, err := strconv.Unquote(line[:end+1])
			if err != nil {
				return nil, fmt.Errorf("%w: malformed go:embed pattern: %w", ErrPatternMalformed, err)
			}
			pattern, line = unquoted, line[end+1:]
		default:
			end := strings.IndexAny(line, " \t")
			if end < 0 {
				pattern, line = line, ""
			} else {
				pattern, line = line[:end], line[end:]
			}
		}
		out = append(out, pattern)
	}
}

// countLines counts the source lines of a file, including a final line that is
// not newline terminated.
func countLines(src []byte) int {
	if len(src) == 0 {
		return 0
	}
	lines := bytes.Count(src, []byte{'\n'})
	if src[len(src)-1] != '\n' {
		lines++
	}
	return lines
}
