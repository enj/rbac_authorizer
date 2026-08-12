package provenance_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

const (
	upstreamSHA    = "3f6c1ad2b1e0b3b2d34f9d31a4f8e7c6d5a49182"
	upstreamModule = "k8s.io/kubernetes"
	upstreamNotice = "Copyright 2014 The Kubernetes Authors.\n\n" +
		"This product includes software developed at\n" +
		"The Linux Foundation (http://www.linuxfoundation.org/).\n"
	upstreamLicense = "                                 Apache License\n" +
		"                           Version 2.0, January 2004\n"
)

// newOptions builds root provenance for the profile's own shape: one extracted
// authorizer, two relocated packages, a prune that removed the scheme
// registration, and no copied dependency.
func newOptions() provenance.Options {
	rewriteOptions := rewrite.Options{
		SourceRepository: "https://github.com/kubernetes/kubernetes.git",
		SourceSHA:        upstreamSHA,
	}
	rbac := rewrite.NewPackageProvenance(
		"internal/kk/plugin/pkg/auth/authorizer/rbac",
		"plugin/pkg/auth/authorizer/rbac", rewriteOptions)
	rbac.AddFile(
		rewrite.File{Path: "internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go", SourcePath: "plugin/pkg/auth/authorizer/rbac/rbac.go"},
		rewrite.Result{Changes: []rewrite.Change{{Kind: rewrite.ChangeImport, Path: "internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go", Line: 22}}})
	rbac.AddPatches("0001-drop-scheme-registration.patch")

	validation := rewrite.NewPackageProvenance(
		"internal/kk/pkg/registry/rbac/validation",
		"pkg/registry/rbac/validation", rewriteOptions)
	validation.AddFile(
		rewrite.File{Path: "internal/kk/pkg/registry/rbac/validation/rule.go", SourcePath: "pkg/registry/rbac/validation/rule.go"},
		rewrite.Result{Changes: []rewrite.Change{{Kind: rewrite.ChangeNotice, Path: "internal/kk/pkg/registry/rbac/validation/rule.go", Line: 1}}})
	validation.AddFile(
		rewrite.File{Path: "internal/kk/pkg/registry/rbac/validation/doc.go", SourcePath: "pkg/registry/rbac/validation/doc.go"},
		rewrite.Result{})
	validation.AddPruned("pkg/registry/rbac/validation/internal_version_adapter.go")
	validation.AddPatches("0001-drop-scheme-registration.patch")

	return provenance.Options{
		Module:         "monis.app/kk/rbac_authorizer",
		RootPackage:    "rbacauthorizer",
		Repository:     "https://github.com/enj/rbac_authorizer",
		InternalPrefix: "internal/kk",
		Summary:        "RBAC authorization extracted from Kubernetes as an independently consumable Go module.",
		Source: provenance.Source{
			Repository: "https://github.com/kubernetes/kubernetes.git",
			Module:     upstreamModule,
			Project:    "Kubernetes",
			SHA:        upstreamSHA,
			Tag:        "v1.36.1",
			Packages:   []string{"plugin/pkg/auth/authorizer/rbac"},
		},
		License:        []byte(upstreamLicense),
		LicenseID:      provenance.Apache20,
		UpstreamNotice: []byte(upstreamNotice),
		Packages:       []*rewrite.PackageProvenance{validation, rbac},
		Modules: []provenance.ModuleMapping{
			{Source: "staging/src/k8s.io/apiserver", Module: "k8s.io/apiserver", Version: "v0.36.1"},
			{Source: "staging/src/k8s.io/api", Module: "k8s.io/api", Version: "v0.36.1"},
		},
		BehaviorChanges: []provenance.BehaviorChange{{
			Summary: "The RBAC API types no longer register themselves into the k8s.io/api scheme at import time.",
			Cause:   "prune",
			Detail:  "The registration file is not reachable from the authorizer path, and removing it stops an import of this module from mutating a scheme the consumer owns.",
		}},
		PublicAPI: []string{"New", "RBACAuthorizer", "RoleGetter"},
	}
}

// TestNoticeEmbedsUpstreamVerbatim is the licence obligation this file exists
// for.
//
// Section 4(d) of the Apache License 2.0 requires the upstream NOTICE to be
// readable in the derivative work. Reproducing it means reproducing it: the
// bytes between the delimiters have to be the upstream file, not a reflowed,
// merged, or summarised version of it.
func TestNoticeEmbedsUpstreamVerbatim(t *testing.T) {
	t.Parallel()
	notice := renderedFile(t, newOptions(), provenance.NoticeFileName)
	embedded := embeddedNotice(t, notice)
	if embedded != upstreamNotice {
		t.Errorf("embedded notice is not the upstream file\ngot:\n%q\nwant:\n%q", embedded, upstreamNotice)
	}
	if !strings.Contains(notice, "BEGIN NOTICE OF "+upstreamModule+" AT "+upstreamSHA) {
		t.Errorf("embedded notice does not say which file and commit it is\n%s", notice)
	}
}

// TestNoticeEmbedsNoticeWithoutTrailingNewline proves the one byte the
// embedding may add, and that it adds no others.
func TestNoticeEmbedsNoticeWithoutTrailingNewline(t *testing.T) {
	t.Parallel()
	options := newOptions()
	options.UpstreamNotice = []byte("Copyright 2014 The Kubernetes Authors.")
	notice := renderedFile(t, options, provenance.NoticeFileName)
	if embedded := embeddedNotice(t, notice); embedded != string(options.UpstreamNotice)+"\n" {
		t.Errorf("embedded notice is %q, want the upstream text with one added terminator", embedded)
	}
}

// TestNoticeRefusesAmbiguousEmbedding proves an upstream notice that contains
// the delimiter is refused rather than embedded, because a reader could not
// then tell where the upstream text ends.
func TestNoticeRefusesAmbiguousEmbedding(t *testing.T) {
	t.Parallel()
	options := newOptions()
	options.UpstreamNotice = []byte("Copyright\n" + strings.Repeat("=", 80) + "\nMore text\n")
	if _, err := options.Files(); !errors.Is(err, provenance.ErrDelimiter) {
		t.Fatalf("files error is %v, want ErrDelimiter", err)
	}
}

// TestNoticeStatesModificationEvidence covers the section 4(b) statement and
// the evidence behind it, including the parts no diff of the copied files would
// show.
func TestNoticeStatesModificationEvidence(t *testing.T) {
	t.Parallel()
	text := flatten(renderedFile(t, newOptions(), provenance.NoticeFileName))
	for _, want := range []string{
		// What the module is and is not.
		"is a derivative work",
		"not a distribution of " + upstreamModule,
		"not produced by, endorsed by, or affiliated with the Kubernetes",
		// Where the code came from.
		"upstream commit: " + upstreamSHA,
		"upstream release: v1.36.1",
		"plugin/pkg/auth/authorizer/rbac",
		// What was changed.
		rewrite.ProvenanceFileName,
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
		"pkg/registry/rbac/validation/internal_version_adapter.go",
		"0001-drop-scheme-registration.patch",
		"no longer register themselves",
		"cause: prune",
		// The staging mapping and the copy decision.
		"staging/src/k8s.io/api -> k8s.io/api@v0.36.1",
		"No dependency package was copied",
		// The trademark position.
		"Section 6 of the Apache License 2.0 grants no rights",
		"is not a Kubernetes release",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("notice does not contain %q\n%s", want, text)
		}
	}

	// A file with no recorded change is not claimed as modified.
	if strings.Contains(text, "validation/doc.go") {
		t.Errorf("notice lists an unchanged file as changed\n%s", text)
	}
	// A patch recorded by two packages is stated once.
	if count := strings.Count(text, "0001-drop-scheme-registration.patch"); count != 1 {
		t.Errorf("notice lists the same patch %d times", count)
	}
}

// TestFilesAreDeterministic proves the committed bytes are a function of the
// inputs alone, and that the licence is reproduced rather than rendered.
func TestFilesAreDeterministic(t *testing.T) {
	t.Parallel()
	options := newOptions()
	first, err := options.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	second, err := options.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}

	names := make([]string, len(first))
	for i, file := range first {
		names[i] = file.Path
		if string(file.Contents) != string(second[i].Contents) {
			t.Errorf("%s differs between renderings", file.Path)
		}
		if file.Mode != relocate.ModeRegular {
			t.Errorf("%s has mode %v, want a regular file", file.Path, file.Mode)
		}
		// A committed artefact that named a directory on the generating machine
		// or read a clock would differ between two otherwise identical runs.
		if strings.Contains(string(file.Contents), os.TempDir()) {
			t.Errorf("%s names a directory on the generating machine", file.Path)
		}
	}
	if strings.Join(names, ",") != "LICENSE,NOTICE,README.md,doc.go" {
		t.Errorf("generated root files are %v, want the four sorted root files", names)
	}
	if string(first[0].Contents) != upstreamLicense {
		t.Errorf("LICENSE is not the upstream licence byte for byte\ngot:\n%q", first[0].Contents)
	}
}

// TestDocIsGeneratedGo proves doc.go is a Go file that carries the package
// documentation and the generated marker in the positions the conventions
// require.
func TestDocIsGeneratedGo(t *testing.T) {
	t.Parallel()
	options := newOptions()
	source := renderedFile(t, options, provenance.DocFileName)
	parsed, err := parser.ParseFile(token.NewFileSet(), "doc.go", source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse doc.go: %v\n%s", err, source)
	}
	if parsed.Name.Name != options.RootPackage {
		t.Errorf("doc.go declares package %s, want %s", parsed.Name.Name, options.RootPackage)
	}
	if !ast.IsGenerated(parsed) {
		t.Errorf("doc.go does not carry the generated file marker\n%s", source)
	}
	if parsed.Doc == nil {
		t.Fatalf("doc.go carries no package documentation\n%s", source)
	}
	doc := flatten(parsed.Doc.Text())
	if !strings.HasPrefix(doc, "Package "+options.RootPackage+" provides "+options.Summary) {
		t.Errorf("package documentation does not begin with the sentence godoc expects: %q", doc)
	}
	for _, want := range []string{upstreamSHA, "not a Kubernetes release", options.InternalPrefix} {
		if !strings.Contains(doc, want) {
			t.Errorf("package documentation does not mention %q\n%s", want, doc)
		}
	}
}

// TestReadmeStatesTheBoundary proves the front page says what a consumer may
// depend on, which is the question a generated module raises and a hand written
// README usually leaves out.
func TestReadmeStatesTheBoundary(t *testing.T) {
	t.Parallel()
	options := newOptions()
	readme := flatten(renderedFile(t, options, provenance.ReadmeFileName))
	for _, want := range []string{
		"# " + options.Module,
		"It is not a Kubernetes release",
		"| upstream commit | `" + upstreamSHA + "` |",
		"import \"" + options.Module + "\"",
		"Only package `" + options.RootPackage + "`",
		"Everything under `" + options.InternalPrefix + "`",
		"- `RBACAuthorizer`",
		rewrite.ProvenanceFileName,
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README does not contain %q\n%s", want, readme)
		}
	}
}

// TestOptionsRefusesUnpublishableInputs covers everything the root files may
// not carry.
//
// The credential cases are the ones that matter most: a clone URL with user
// information is an ordinary thing to have in a shell history, and writing one
// into a committed NOTICE publishes it to everyone who ever reads the module.
func TestOptionsRefusesUnpublishableInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*provenance.Options)
		want   error
	}{
		{
			name: "credential in the upstream repository URL",
			mutate: func(o *provenance.Options) {
				o.Source.Repository = "https://user:token@github.com/kubernetes/kubernetes.git"
			},
			want: provenance.ErrSecret,
		},
		{
			name: "credential in the module repository URL",
			mutate: func(o *provenance.Options) {
				o.Repository = "https://x-access-token:ghs_secret@github.com/enj/rbac_authorizer"
			},
			want: provenance.ErrSecret,
		},
		{
			name:   "plaintext repository URL",
			mutate: func(o *provenance.Options) { o.Repository = "http://github.com/enj/rbac_authorizer" },
			want:   provenance.ErrOptions,
		},
		{
			name:   "absolute internal prefix",
			mutate: func(o *provenance.Options) { o.InternalPrefix = "/home/build/internal/kk" },
			want:   provenance.ErrOptions,
		},
		{
			name:   "absolute upstream package path",
			mutate: func(o *provenance.Options) { o.Source.Packages = []string{"/src/plugin/pkg"} },
			want:   provenance.ErrOptions,
		},
		{
			name:   "upstream commit that is not an object name",
			mutate: func(o *provenance.Options) { o.Source.SHA = "v1.36.1" },
			want:   provenance.ErrOptions,
		},
		{
			name:   "missing licence",
			mutate: func(o *provenance.Options) { o.License = nil },
			want:   provenance.ErrOptions,
		},
		{
			name: "licence text that is not the licence it claims",
			mutate: func(o *provenance.Options) {
				o.License = []byte("MIT License\n\nPermission is hereby granted, free of charge\n")
			},
			want: provenance.ErrLicense,
		},
		{
			name:   "licence identifier this engine cannot verify",
			mutate: func(o *provenance.Options) { o.LicenseID = "LicenseRef-Custom" },
			want:   provenance.ErrOptions,
		},
		{
			name:   "no relocated package recorded",
			mutate: func(o *provenance.Options) { o.Packages = nil },
			want:   provenance.ErrEvidence,
		},
		{
			name:   "licence that is not text",
			mutate: func(o *provenance.Options) { o.License = []byte{0xff, 0xfe, 0x00} },
			want:   provenance.ErrOptions,
		},
		{
			name:   "no summary",
			mutate: func(o *provenance.Options) { o.Summary = "" },
			want:   provenance.ErrOptions,
		},
		{
			name:   "staging mapping without a version",
			mutate: func(o *provenance.Options) { o.Modules[0].Version = "" },
			want:   provenance.ErrOptions,
		},
		{
			name:   "behaviour change without a summary",
			mutate: func(o *provenance.Options) { o.BehaviorChanges[0].Summary = " " },
			want:   provenance.ErrOptions,
		},
		{
			name: "copied package with no licence",
			mutate: func(o *provenance.Options) {
				o.Copied = []provenance.CopiedPackage{{
					Module: "k8s.io/apiserver", Version: "v0.36.1",
					Package:          "k8s.io/apiserver/pkg/authorization/authorizer",
					Destination:      "internal/kk/staging/src/k8s.io/apiserver/pkg/authorization/authorizer",
					SourceRepository: "https://github.com/kubernetes/kubernetes.git",
					SourceSHA:        upstreamSHA,
					LicenseID:        provenance.Apache20,
				}}
			},
			want: provenance.ErrOptions,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := newOptions()
			test.mutate(&options)
			if _, err := options.Files(); !errors.Is(err, test.want) {
				t.Fatalf("files error is %v, want %v", err, test.want)
			}
		})
	}
}

// TestNoticeRecordsCopiedPackages proves the copied dependency section states a
// complete record when a profile does copy something.
func TestNoticeRecordsCopiedPackages(t *testing.T) {
	t.Parallel()
	options := newOptions()
	options.Copied = []provenance.CopiedPackage{{
		Module:           "k8s.io/apiserver",
		Version:          "v0.36.1",
		Package:          "k8s.io/apiserver/pkg/authorization/authorizer",
		Destination:      "internal/kk/staging/src/k8s.io/apiserver/pkg/authorization/authorizer",
		SourceRepository: "https://github.com/kubernetes/kubernetes.git",
		SourceSHA:        upstreamSHA,
		Override:         "copy-apiserver-2026-05-01",
		LicenseID:        provenance.Apache20,
		Licenses: []provenance.LicenseFile{{
			Name:        "LICENSE",
			SourcePath:  "staging/src/k8s.io/apiserver/LICENSE",
			Destination: "internal/kk/staging/src/k8s.io/apiserver/LICENSE",
			Contents:    []byte(upstreamLicense),
			SHA256:      "0f5b1c0e6d4a2f3b8c7d9e0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e",
		}},
	}}
	text := flatten(renderedFile(t, options, provenance.NoticeFileName))
	for _, want := range []string{
		"k8s.io/apiserver/pkg/authorization/authorizer",
		"module: k8s.io/apiserver@v0.36.1",
		"approval: copy-apiserver-2026-05-01",
		"licence: " + provenance.Apache20,
		"file: internal/kk/staging/src/k8s.io/apiserver/LICENSE",
		"sha256 0f5b1c0e",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("notice does not contain %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "No dependency package was copied") {
		t.Errorf("notice claims nothing was copied while listing a copy\n%s", text)
	}
}

// renderedFile reports one generated root file.
//
// Everything goes through Files, which is the package's one entry point and the
// only place inputs are validated: a test that reached a renderer directly
// would be exercising a path no caller has.
func renderedFile(t *testing.T, options provenance.Options, name string) string {
	t.Helper()
	files, err := options.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	for _, file := range files {
		if file.Path == name {
			return string(file.Contents)
		}
	}
	t.Fatalf("root provenance does not include %s", name)
	return ""
}

// flatten collapses every run of whitespace into one space.
//
// The generated prose is wrapped at a fixed width, so a sentence a test looks
// for is usually split across lines. Comparing against the flattened text keeps
// the assertions about what the file says rather than about where it happened
// to wrap, which the wrapping tests cover on their own.
func flatten(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// embeddedNotice extracts the text between the embedding delimiters.
func embeddedNotice(t *testing.T, notice string) string {
	t.Helper()
	const rule = "================================================================================"
	begin := strings.Index(notice, "BEGIN NOTICE OF ")
	if begin < 0 {
		t.Fatalf("notice has no embedded upstream section\n%s", notice)
	}
	// The embedded text starts after the rule that follows the BEGIN line.
	start := strings.Index(notice[begin:], "\n"+rule+"\n")
	if start < 0 {
		t.Fatalf("notice has no opening delimiter\n%s", notice)
	}
	start += begin + len("\n"+rule+"\n")
	end := strings.Index(notice[start:], rule+"\nEND NOTICE OF ")
	if end < 0 {
		t.Fatalf("notice has no closing delimiter\n%s", notice)
	}
	return notice[start : start+end]
}

// writeFile writes one fixture file, creating its directories.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
