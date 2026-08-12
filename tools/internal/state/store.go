package state

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// File is the single path a stored record occupies in its own tree.
//
// The record lives in a tree of its own rather than beside the generated
// module, so nothing about it can reach a module consumer: it is not in the
// published tree, it is not in the module zip the proxy serves, and no import
// path resolves into it.
const File = "state.json"

// StoreOptions describes one stored record.
//
// The identity and the dates are supplied rather than read from configuration
// or from the clock, because they are inputs to the commit's object name. A
// commit that took its date from the clock would have a different name on every
// run, and the property this package exists to provide is that a run which did
// the same work writes the same objects.
type StoreOptions struct {
	// Document is the record to store.
	Document Document
	// Parents are the previous state commits, in order. A first record has none.
	Parents []string
	// Author is the identity the record is attributed to, with a raw date.
	Author gitcli.Signature
	// Committer is the identity that recorded it, with a raw date.
	Committer gitcli.Signature
}

// Record names the objects one stored document produced.
type Record struct {
	// Format is the hash algorithm the objects were written under.
	Format gitcli.ObjectFormat
	// Blob is the object holding the encoded document.
	Blob string
	// Tree is the object holding the blob at File.
	Tree string
	// Commit is the object holding the tree. No ref points at it.
	Commit string
	// Digest is the document's own digest, repeated here so a caller comparing
	// two runs does not have to decode either one.
	Digest string
	// Bytes is the encoded length of the document.
	Bytes int64
}

// Report renders the record as deterministic lines for a dry run.
//
// Two runs that recorded the same state produce identical lines, and the lines
// carry nothing about the machine that produced them, so a person approving an
// outward plan is comparing the work rather than the runner.
func (r Record) Report() []string {
	return []string{
		"format " + string(r.Format),
		"digest " + r.Digest,
		"blob " + r.Blob + " bytes " + strconv.FormatInt(r.Bytes, 10),
		"tree " + r.Tree,
		"commit " + r.Commit,
	}
}

// Store writes a document as a blob, a tree, and a commit, and reports their
// names.
//
// No ref is created, moved, or deleted. That is the whole point of the seam: a
// run persists where it got to by writing objects, and whether any branch
// should point at the newest one is a publication decision made after every
// gate has passed. Until that decision is made the commit is unreachable, which
// is exactly what a record of work in progress should be.
//
// The stored bytes are read back out of the commit and checked against the
// bytes that went in, and the document is decoded from them and checked against
// its own digest. A write that git accepted and altered, through a filter or a
// conversion, is a resume from a record nobody produced, and the only place to
// notice is here.
func Store(ctx context.Context, git *gitcli.Runner, opts StoreOptions) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	encoded, err := opts.Document.Encode()
	if err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	if err := requireFormat(ctx, git, opts.Document.ObjectFormat); err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	if err := checkParents(ctx, git, opts.Parents, opts.Document.ObjectFormat); err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}

	manifest, err := treebuild.WriteFileSet(ctx, git, relocate.FileSet{Files: []relocate.File{{
		Path:     File,
		Mode:     relocate.ModeRegular,
		Contents: encoded,
	}}})
	if err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}
	if len(manifest.Files) != 1 {
		return Record{}, fmt.Errorf("store state: the record tree holds %d files, want 1", len(manifest.Files))
	}

	commit, err := treebuild.WriteSyntheticCommit(ctx, git, treebuild.SyntheticCommitOptions{
		Tree:      manifest.Tree,
		Parents:   opts.Parents,
		Author:    opts.Author,
		Committer: opts.Committer,
		Message:   message(opts.Document),
	})
	if err != nil {
		return Record{}, fmt.Errorf("store state: %w", err)
	}

	stored, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: commit, Path: File})
	if err != nil {
		return Record{}, fmt.Errorf("store state: read back: %w", err)
	}
	if !bytes.Equal(stored, encoded) {
		return Record{}, fmt.Errorf("store state: commit %s holds %d bytes at %s, %d were written",
			commit, len(stored), File, len(encoded))
	}
	readBack, err := Decode(stored)
	if err != nil {
		return Record{}, fmt.Errorf("store state: read back: %w", err)
	}
	if readBack.Digest != opts.Document.Digest {
		return Record{}, fmt.Errorf("%w: commit %s holds %s, %s was written",
			ErrDigest, commit, readBack.Digest, opts.Document.Digest)
	}

	return Record{
		Format: manifest.Format,
		Blob:   manifest.Files[0].Object,
		Tree:   manifest.Tree,
		Commit: commit,
		Digest: readBack.Digest,
		Bytes:  manifest.Files[0].Size,
	}, nil
}

// Load reads the document a revision holds.
//
// The revision may be a commit or a tree. It is resolved through the object
// store rather than through a ref, so a caller that already knows which commit
// it wants to resume from does not have to have published it anywhere.
func Load(ctx context.Context, git *gitcli.Runner, revision string) (Document, error) {
	if err := ctx.Err(); err != nil {
		return Document{}, fmt.Errorf("load state: %w", err)
	}
	encoded, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: revision, Path: File})
	if err != nil {
		return Document{}, fmt.Errorf("load state: %w", err)
	}
	doc, err := Decode(encoded)
	if err != nil {
		return Document{}, fmt.Errorf("load state %s: %w", revision, err)
	}
	// The document names the hash algorithm every object in it is written in,
	// so a record from another repository decodes cleanly and still describes
	// commits this one does not have. Comparing it against the repository is
	// what turns that into a refusal instead of a resume from nothing.
	if err := requireFormat(ctx, git, doc.ObjectFormat); err != nil {
		return Document{}, fmt.Errorf("load state %s: %w", revision, err)
	}
	return doc, nil
}

// requireFormat reports a document whose object names belong to a repository
// with a different hash algorithm.
func requireFormat(ctx context.Context, git *gitcli.Runner, want gitcli.ObjectFormat) error {
	format, err := git.ObjectFormat(ctx)
	if err != nil {
		return err
	}
	if format != want {
		return fmt.Errorf("%w: the document records %s object names, the repository writes %s",
			ErrObjectFormat, string(want), string(format))
	}
	return nil
}

// checkParents reports a parent that is not a commit this repository holds
// under this document's hash algorithm.
//
// Three things are refused and each would produce a commit that reads back
// wrong rather than a call that fails. An abbreviation, or any name that is not
// the document's width, would let a sha1 name into a sha256 record: git resolves
// it or does not, and either way the state chain would name a parent in a
// notation the rest of the document does not use. A name for an object that is
// not here would produce a chain whose earlier records cannot be read. And an
// object that is here but is a tree or a blob would make the state history
// unwalkable at exactly the point a resume needs to walk it.
//
// The probe answers from the local object store only, which is what "do I hold
// this" means. A partial clone must not reach the network to decide whether a
// record it wrote earlier exists.
func checkParents(ctx context.Context, git *gitcli.Runner, parents []string, format gitcli.ObjectFormat) error {
	if len(parents) == 0 {
		return nil
	}
	width := format.HexLength()
	for i, parent := range parents {
		if err := checkObject(fmt.Sprintf("parent %d", i), parent, width); err != nil {
			return err
		}
	}
	infos, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: parents})
	if err != nil {
		return err
	}
	if len(infos) != len(parents) {
		return fmt.Errorf("probed %d parents, got %d answers", len(parents), len(infos))
	}
	for i, info := range infos {
		switch {
		case info.Missing:
			return fmt.Errorf("parent %d %s is not an object this repository holds", i, parents[i])
		case info.Type != "commit":
			return fmt.Errorf("parent %d %s is a %s, not a commit", i, parents[i], info.Type)
		}
	}
	return nil
}

// message renders the commit message for a stored record.
//
// It is derived from the document rather than taken from the caller, so the
// commit is a function of the record, its parents, and the two signatures, and
// nothing else. A caller supplied message would be one more input that two runs
// doing the same work could differ on.
//
// Neither body line has the shape of a trailer, which keeps the message free of
// provenance a reader could mistake for a claim about an upstream commit. A
// state commit is not a transformed commit and must not be mapped back to one.
func message(doc Document) string {
	return "soapbox state\n\nschema " + strconv.Itoa(doc.Schema) + "\ndigest " + doc.Digest + "\n"
}
