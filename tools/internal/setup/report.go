package setup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// ReportSchema is the manifest schema version. It changes whenever the meaning
// of a field changes, because an approval hash is only comparable between two
// manifests that were computed the same way.
const ReportSchema = 1

// The kinds of change a manifest records.
const (
	// ActionCreate writes a path that does not exist.
	ActionCreate = "create"
	// ActionReplace writes over a path the template owns.
	ActionReplace = "replace"
	// ActionDelete removes a path the template owns.
	ActionDelete = "delete"
)

// Report is the manifest of one setup.
//
// Every path in it is repository relative and separated by forward slashes. No
// absolute directory appears anywhere, which is what lets two operators on two
// machines compare hashes and lets a test assert that the same template produces
// the same manifest from two different temporary directories.
type Report struct {
	// Schema is the manifest schema version.
	Schema int `json:"schema"`
	// Engine describes the release the nested tools module will pin.
	Engine Engine `json:"engine"`
	// Module describes the derived repository's identity.
	Module Module `json:"module"`
	// Actions are every write and delete, sorted by path.
	Actions []Action `json:"actions"`
	// Kept are the tracked paths the derived repository keeps untouched.
	Kept []string `json:"kept"`
	// Ignored are the tracked paths setup does not recognise and therefore
	// preserves. A file the operator added lands here.
	Ignored []string `json:"ignored"`
	// Totals count the manifest by kind.
	Totals Totals `json:"totals"`
	// Notices are the things an operator has to do that setup could not.
	Notices []string `json:"notices"`
	// Hash is the approval hash over everything above it.
	Hash string `json:"hash"`
}

// Engine is the pinned engine release.
type Engine struct {
	// Version is the engine that produced this manifest.
	Version string `json:"version"`
	// Module is the engine module the shim requires.
	Module string `json:"module"`
	// Require is the version the shim's go.mod names.
	Require string `json:"require"`
	// Tag is the repository tag that publishes it.
	Tag string `json:"tag"`
	// Toolchain is the exact Go release the generated modules pin.
	Toolchain string `json:"toolchain"`
	// Go is the language version the generated modules declare.
	Go string `json:"go"`
	// Sum reports whether verified checksums were supplied and written.
	Sum bool `json:"sum"`
}

// Module is the derived repository's identity.
type Module struct {
	// Path is the root module path consumers import.
	Path string `json:"path"`
	// Tools is the nested tools module path.
	Tools string `json:"tools"`
	// Repository is the GitHub repository the profile names.
	Repository string `json:"repository"`
	// Branch is the protected default branch publishing runs from.
	Branch string `json:"branch"`
}

// Action is one write or delete.
type Action struct {
	// Path is the repository relative path.
	Path string `json:"path"`
	// Kind is create, replace, or delete.
	Kind string `json:"kind"`
	// Digest is the content written, or for a delete the content removed. A
	// deletion names what it destroys so an approval covers the bytes rather
	// than the file name.
	Digest string `json:"digest"`
	// Bytes is the length of that content.
	Bytes int `json:"bytes"`
}

// Totals count a manifest by kind.
type Totals struct {
	Create  int `json:"create"`
	Replace int `json:"replace"`
	Delete  int `json:"delete"`
	Kept    int `json:"kept"`
	Ignored int `json:"ignored"`
}

// JSON renders the manifest canonically.
func (r Report) JSON() ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		return nil, fmt.Errorf("setup report: %w", err)
	}
	return out.Bytes(), nil
}

// computeHash renders the approval hash.
//
// The hash covers the manifest with its own hash field empty, so a report can
// carry the hash of itself without the hash depending on where it was written.
// Everything else is included, deletions and notices among them, because the
// operator approves the whole answer rather than the part of it that writes.
func (r Report) computeHash() (string, error) {
	r.Hash = ""
	encoded, err := r.JSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Summary renders the manifest for a person.
//
// It names no absolute directory, for the same reason the JSON does not: an
// operator comparing two dry runs should see a difference only where the
// repositories differ.
func (r *Result) Summary() string {
	var b strings.Builder
	report := r.Report

	verb := "would change"
	if r.Applied {
		verb = "changed"
	}
	fmt.Fprintf(&b, "soapbox setup %s %s\n", verb, report.Module.Repository)
	fmt.Fprintf(&b, "  module        %s\n", report.Module.Path)
	fmt.Fprintf(&b, "  tools module  %s\n", report.Module.Tools)
	fmt.Fprintf(&b, "  engine        %s pinned at %s, toolchain %s\n", report.Engine.Module, report.Engine.Tag, report.Engine.Toolchain)
	fmt.Fprintf(&b, "  checksums     %s\n", checksumState(report.Engine.Sum))
	fmt.Fprintf(&b, "  writes        %d created, %d replaced\n", report.Totals.Create, report.Totals.Replace)
	fmt.Fprintf(&b, "  deletes       %d template files\n", report.Totals.Delete)
	fmt.Fprintf(&b, "  preserved     %d kept, %d untouched and unrecognised\n", report.Totals.Kept, report.Totals.Ignored)

	// Every action is listed, deletions included. The deletions are the part an
	// operator is really approving, and a summary that only counted them would
	// leave the one destructive half of the manifest readable in JSON alone.
	for _, action := range report.Actions {
		fmt.Fprintf(&b, "  %-13s %s\n", action.Kind, action.Path)
	}
	for _, path := range report.Ignored {
		fmt.Fprintf(&b, "  preserved     %s\n", path)
	}
	for _, notice := range report.Notices {
		fmt.Fprintf(&b, "  notice        %s\n", notice)
	}
	fmt.Fprintf(&b, "  manifest      %s\n", report.Hash)
	switch {
	case r.Partial:
		fmt.Fprintln(&b, "\napply failed after the repository may have changed; inspect or reset it before retrying.")
	case !r.Applied:
		fmt.Fprintf(&b, "\nnothing was written. to apply exactly this manifest:\n  soapbox setup -apply -approve %s\n", report.Hash)
	}
	return b.String()
}

// checksumState renders whether the nested module carries verified checksums.
func checksumState(written bool) string {
	if written {
		return "written from the supplied verified go.sum"
	}
	return "not written, see notices"
}
