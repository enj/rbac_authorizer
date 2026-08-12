package treebuild_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// newRepo creates a real repository whose objects use the named hash algorithm.
//
// HOME is redirected through Isolation rather than Env so the runner stays
// anonymous: the redirection decides where git looks for state and carries no
// credential.
func newRepo(ctx context.Context, t *testing.T, format gitcli.ObjectFormat) (*gitcli.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	git, err := gitcli.New(ctx, gitcli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH"},
		Isolation: []string{"HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepositoryWithFormat(ctx, "main", format); err != nil {
		t.Fatalf("init %s repository: %v", format, err)
	}
	return git, dir
}

// file builds one relocated file.
func file(path string, mode relocate.Mode, contents string) relocate.File {
	return relocate.File{Path: path, Mode: mode, Contents: []byte(contents)}
}

// set builds a relocated file set from files given in any order.
func set(files ...relocate.File) relocate.FileSet {
	return relocate.FileSet{Files: files}
}

// writeTree builds a small tree and reports its object name, for the tests whose
// subject is a commit or a tag rather than the tree itself.
func writeTree(ctx context.Context, t *testing.T, git *gitcli.Runner) string {
	t.Helper()
	manifest, err := treebuild.WriteFileSet(ctx, git, set(
		file("internal/kk/pkg/apis/rbac/types.go", relocate.ModeRegular, "package rbac\n"),
	))
	if err != nil {
		t.Fatalf("write file set: %v", err)
	}
	return manifest.Tree
}

// objectFormats are the hash algorithms every object level property is proved
// under. sha256 is not hypothetical: it is a repository creation time choice
// that cannot be changed afterwards, so a computed object name that only ever
// ran under sha1 would be wrong in exactly the repository nobody tested.
var objectFormats = []gitcli.ObjectFormat{gitcli.ObjectFormatSHA1, gitcli.ObjectFormatSHA256}

// TestWriteFileSetIsDeterministic pins the property the published module rests
// on: the same content produces the same tree, whatever order it arrived in and
// however many times it is built.
//
// The reversal is the real assertion. Nothing in the engine guarantees the order
// a closure hands files over in, so a tree that depended on it would be
// reproducible only by accident.
func TestWriteFileSetIsDeterministic(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			git, _ := newRepo(ctx, t, format)

			files := []relocate.File{
				file("internal/kk/pkg/apis/rbac/types.go", relocate.ModeRegular, "package rbac\n"),
				file("internal/kk/pkg/registry/rule.go", relocate.ModeRegular, "package registry\n"),
				file("hack/build.sh", relocate.ModeExecutable, "#!/bin/sh\nexit 0\n"),
				file("LICENSE", relocate.ModeRegular, "Apache-2.0\n"),
			}
			forward, err := treebuild.WriteFileSet(ctx, git, set(files...))
			if err != nil {
				t.Fatalf("write file set: %v", err)
			}
			if len(forward.Tree) != format.HexLength() {
				t.Fatalf("tree %q is not a %s object name", forward.Tree, format)
			}
			if forward.Written != len(files) || forward.Reused != 0 {
				t.Fatalf("first build wrote %d and reused %d blobs, want %d written", forward.Written, forward.Reused, len(files))
			}

			reversed := slices.Clone(files)
			slices.Reverse(reversed)
			backward, err := treebuild.WriteFileSet(ctx, git, set(reversed...))
			if err != nil {
				t.Fatalf("write reversed file set: %v", err)
			}
			if backward.Tree != forward.Tree {
				t.Fatalf("reversed input produced tree %s, want %s", backward.Tree, forward.Tree)
			}
			// Every blob is already present the second time, which is what keeps
			// a replay from rewriting an unchanged file once per commit.
			if backward.Written != 0 || backward.Reused != len(files) {
				t.Fatalf("second build wrote %d and reused %d blobs, want all reused", backward.Written, backward.Reused)
			}
			if !slices.Equal(forward.Report(), backward.Report()) {
				t.Fatalf("reports differ:\n%s\nand\n%s",
					strings.Join(forward.Report(), "\n"), strings.Join(backward.Report(), "\n"))
			}
		})
	}
}

// TestWriteFileSetPreservesPaths checks that a path survives the round trip
// through the index framing exactly as written.
//
// The framing is null delimited and the path follows a tab, so a space, a tab, a
// quote, or a byte outside ASCII in a path is only safe if nothing along the way
// quotes, splits, or re-encodes it. Upstream Kubernetes has testdata with all of
// them, and a path that came back different would publish a file under a name
// nobody chose.
func TestWriteFileSetPreservesPaths(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	paths := []string{
		"internal/kk/testdata/a file with spaces.txt",
		"internal/kk/testdata/a\tfile\twith\ttabs.txt",
		"internal/kk/testdata/a\"quoted\".txt",
		"internal/kk/testdata/'single'.txt",
		"internal/kk/testdata/café-ünïcode-日本語.txt",
		"internal/kk/testdata/back`tick.txt",
		"internal/kk/testdata/dollar$sign.txt",
		"internal/kk/testdata/.gitignore",
		"internal/kk/testdata/.gitattributes",
	}
	files := make([]relocate.File, 0, len(paths))
	for i, path := range paths {
		files = append(files, file(path, relocate.ModeRegular, "content "+string(rune('a'+i))+"\n"))
	}

	manifest, err := treebuild.WriteFileSet(ctx, git, set(files...))
	if err != nil {
		t.Fatalf("write file set: %v", err)
	}
	// WriteFileSet already compares the tree it read back against the entries it
	// wrote, so reaching here means every path survived. Reading it again keeps
	// the assertion in the test rather than only inside the code under test.
	listed, err := git.ListTree(ctx, manifest.Tree)
	if err != nil {
		t.Fatalf("list tree: %v", err)
	}
	got := make([]string, 0, len(listed))
	for _, entry := range listed {
		got = append(got, entry.Path)
	}
	want := slices.Clone(paths)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("tree holds paths\n%q\nwant\n%q", got, want)
	}
}

// TestWriteFileSetRecordsModes checks that each of the three modes a generated
// tree may hold reaches the tree as itself.
//
// The executable and the regular file share one content, which is also the
// dedupe assertion: git stores the mode in the tree entry rather than in the
// blob, so the same bytes are one object no matter how many modes name it.
func TestWriteFileSetRecordsModes(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	const shared = "#!/bin/sh\nexit 0\n"
	manifest, err := treebuild.WriteFileSet(ctx, git, set(
		file("plain.sh", relocate.ModeRegular, shared),
		file("runnable.sh", relocate.ModeExecutable, shared),
		file("link", relocate.ModeSymlink, "runnable.sh"),
	))
	if err != nil {
		t.Fatalf("write file set: %v", err)
	}

	want := map[string]gitcli.FileMode{
		"plain.sh":    gitcli.ModeRegular,
		"runnable.sh": gitcli.ModeExecutable,
		"link":        gitcli.ModeSymlink,
	}
	listed, err := git.ListTree(ctx, manifest.Tree)
	if err != nil {
		t.Fatalf("list tree: %v", err)
	}
	if len(listed) != len(want) {
		t.Fatalf("tree holds %d entries, want %d", len(listed), len(want))
	}
	byPath := make(map[string]gitcli.TreeEntry, len(listed))
	for _, entry := range listed {
		byPath[entry.Path] = entry
	}
	for path, mode := range want {
		entry, found := byPath[path]
		if !found {
			t.Fatalf("tree has no entry for %q", path)
		}
		if entry.Mode != mode {
			t.Fatalf("%q has mode %q, want %q", path, string(entry.Mode), string(mode))
		}
	}
	if byPath["plain.sh"].Object != byPath["runnable.sh"].Object {
		t.Fatal("identical content was stored as two blobs")
	}
	if manifest.Written != 2 {
		t.Fatalf("wrote %d blobs, want 2 for three files sharing one content", manifest.Written)
	}
	// A symbolic link's blob is its target, which is what makes the link mean
	// the same thing wherever the tree is checked out.
	target, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: byPath["link"].Object})
	if err != nil {
		t.Fatalf("read link blob: %v", err)
	}
	if string(target) != "runnable.sh" {
		t.Fatalf("link blob holds %q, want the link target", target)
	}
}

// TestWriteFileSetHandlesBinaryContent checks that content is stored byte for
// byte, including the bytes that would not survive being treated as text.
//
// Kubernetes carries binary testdata and generated protobuf, so this is not a
// hypothetical: a blob that gained a line ending conversion or lost a null would
// be a different file with the same name.
func TestWriteFileSetHandlesBinaryContent(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			git, _ := newRepo(ctx, t, format)

			contents := map[string][]byte{
				"empty":        {},
				"nulls":        {0x00, 0x01, 0x00, 0xff},
				"invalid-utf8": {0xc3, 0x28, 0xa0, 0xa1},
				"crlf":         []byte("line one\r\nline two\r\n"),
				"no-newline":   []byte("no trailing newline"),
			}
			files := make([]relocate.File, 0, len(contents))
			for path, content := range contents {
				files = append(files, relocate.File{Path: path, Mode: relocate.ModeRegular, Contents: content})
			}
			manifest, err := treebuild.WriteFileSet(ctx, git, set(files...))
			if err != nil {
				t.Fatalf("write file set: %v", err)
			}
			listed, err := git.ListTree(ctx, manifest.Tree)
			if err != nil {
				t.Fatalf("list tree: %v", err)
			}
			for _, entry := range listed {
				got, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: entry.Object})
				if err != nil {
					t.Fatalf("read %q: %v", entry.Path, err)
				}
				if !bytes.Equal(got, contents[entry.Path]) {
					t.Fatalf("%q round tripped as %q, want %q", entry.Path, got, contents[entry.Path])
				}
			}
		})
	}
}

// TestWriteFileSetIgnoresRepositoryConversionConfig checks that a repository
// configured to rewrite line endings cannot change what is published.
//
// This is the failure that would be hardest to notice and worst to ship. A
// developer with core.autocrlf set, or a repository carrying a .gitattributes
// that asks for conversion, would otherwise produce a module whose bytes differ
// from the one CI produced, and both would look correct locally. The tree name
// is compared against a repository with no such configuration, so the assertion
// is that the two are the same object rather than merely that each is internally
// consistent.
func TestWriteFileSetIgnoresRepositoryConversionConfig(t *testing.T) {
	ctx := t.Context()

	files := []relocate.File{
		file("crlf.go", relocate.ModeRegular, "package kk\r\n\r\nfunc F() {}\r\n"),
		file("lf.go", relocate.ModeRegular, "package kk\n\nfunc G() {}\n"),
		file(".gitattributes", relocate.ModeRegular, "* text=auto eol=crlf\n"),
	}

	plain, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	want, err := treebuild.WriteFileSet(ctx, plain, set(files...))
	if err != nil {
		t.Fatalf("write file set in a plain repository: %v", err)
	}

	converting, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	for key, value := range map[string]string{
		"core.autocrlf": "true",
		"core.eol":      "crlf",
		"core.safecrlf": "false",
	} {
		if err := converting.SetConfigLocal(ctx, key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	got, err := treebuild.WriteFileSet(ctx, converting, set(files...))
	if err != nil {
		t.Fatalf("write file set in a converting repository: %v", err)
	}

	if got.Tree != want.Tree {
		t.Fatalf("a repository configured to convert line endings produced tree %s, want %s", got.Tree, want.Tree)
	}
	// The bytes themselves, not only the tree name, so a failure says which
	// file was rewritten rather than only that something was.
	sourceBytes := make(map[string][]byte, len(files))
	for _, source := range files {
		sourceBytes[source.Path] = source.Contents
	}
	for _, entry := range got.Files {
		blob, err := converting.ReadBlob(ctx, gitcli.BlobOptions{Revision: entry.Object})
		if err != nil {
			t.Fatalf("read %q: %v", entry.Path, err)
		}
		source, found := sourceBytes[entry.Path]
		if !found {
			t.Fatalf("the tree gained an entry for %q", entry.Path)
		}
		if !bytes.Equal(blob, source) {
			t.Fatalf("%q was stored as %q, want %q", entry.Path, blob, source)
		}
	}
}

// TestWriteFileSetTouchesNoWorkTreeOrRefs pins that building objects publishes
// nothing.
//
// A tree, its blobs, and a commit are all reachable from nothing until a ref
// names them, and that separation is what lets a dry run compute exactly what it
// would publish without having published it. A build that checked a file out or
// moved a branch would have made a claim the operator has not approved yet.
func TestWriteFileSetTouchesNoWorkTreeOrRefs(t *testing.T) {
	ctx := t.Context()
	git, dir := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	manifest, err := treebuild.WriteFileSet(ctx, git, set(
		file("internal/kk/pkg/apis/rbac/types.go", relocate.ModeRegular, "package rbac\n"),
	))
	if err != nil {
		t.Fatalf("write file set: %v", err)
	}
	if _, err := treebuild.WriteSyntheticCommit(ctx, git, treebuild.SyntheticCommitOptions{
		Tree:      manifest.Tree,
		Message:   "chore: generated tree\n",
		Author:    gitcli.Signature{Name: "Bot", Email: "bot@example.com", Date: "1700000000 +0000"},
		Committer: gitcli.Signature{Name: "Bot", Email: "bot@example.com", Date: "1700000000 +0000"},
	}); err != nil {
		t.Fatalf("write commit: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read repository directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			t.Fatalf("the work tree gained %q, and object writing must not check anything out", entry.Name())
		}
	}
	head, err := git.HasHead(ctx)
	if err != nil {
		t.Fatalf("head probe: %v", err)
	}
	if head {
		t.Fatal("a branch was moved, and object writing must create no ref")
	}
}

// TestWriteFileSetRejects covers the inputs that must fail rather than produce a
// tree that does not describe the module.
func TestWriteFileSetRejects(t *testing.T) {
	tests := []struct {
		name string
		set  relocate.FileSet
		want error
	}{
		{
			name: "no files",
			set:  relocate.FileSet{},
		},
		{
			name: "mode a tree cannot record",
			set:  set(relocate.File{Path: "device", Mode: relocate.Mode(99), Contents: []byte("x")}),
			want: treebuild.ErrUnsupportedMode,
		},
		{
			name: "two files on one path",
			set: set(
				file("same.go", relocate.ModeRegular, "one\n"),
				file("same.go", relocate.ModeRegular, "two\n"),
			),
			want: gitcli.ErrDuplicateTreeEntry,
		},
		{
			name: "a file that is also a directory",
			set: set(
				file("pkg", relocate.ModeRegular, "one\n"),
				file("pkg/inner.go", relocate.ModeRegular, "two\n"),
			),
			want: gitcli.ErrTreePathConflict,
		},
		{
			// git stages neither of these and says so only on standard error
			// while exiting zero, so the tree would come back quietly missing a
			// file. The verdict has to reach the caller of this package intact.
			name: "a path naming git's own directory",
			set: set(
				file("internal/kk/.git/config", relocate.ModeRegular, "one\n"),
			),
			want: gitcli.ErrReservedTreePath,
		},
		{
			name: "a path escaping the tree",
			set: set(
				file("../outside.go", relocate.ModeRegular, "one\n"),
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

			manifest, err := treebuild.WriteFileSet(ctx, git, test.set)
			if err == nil {
				t.Fatalf("the set was accepted and produced tree %s", manifest.Tree)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error %v does not wrap %v", err, test.want)
			}
		})
	}
}

// TestWriteFileSetHonoursCancellation checks that a cancelled build reports the
// cancellation rather than a partial tree.
func TestWriteFileSetHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	cancel()

	manifest, err := treebuild.WriteFileSet(ctx, git, set(
		file("internal/kk/types.go", relocate.ModeRegular, "package kk\n"),
	))
	if err == nil {
		t.Fatalf("a cancelled build produced tree %s", manifest.Tree)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not wrap context.Canceled", err)
	}
}
