package generate_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// The staging modules are spelled under a reserved test domain rather than under
// k8s.io. The module cache is shared across every test in this package and is
// keyed by module path and version, so a fixture that called itself k8s.io/api
// at v0.36.1 would be indistinguishable from the real one and either would serve
// the other. A domain nothing publishes under cannot collide.
const (
	stagingAPI       = "soapbox.test/api"
	stagingAPIServer = "soapbox.test/apiserver"
)

// upstreamFiles is the miniature Kubernetes-shaped source tree every end-to-end
// test extracts from.
//
// It is small enough to read and shaped exactly like the part of Kubernetes the
// real profile touches: a root package that authorizes, a validation package it
// depends on, a versioned API helper package, and an unversioned internal API
// package that only the pruned registration file reaches. That last edge is the
// whole point of the fixture: removing one file drops one package out of the
// closure, which is the change the facade comparison and the type policy are
// asked to prove is invisible.
var upstreamFiles = map[string]string{
	"go.mod": "module k8s.io/kubernetes\n" +
		"\n" +
		"go 1.26.0\n" +
		"\n" +
		"require (\n" +
		"\t" + stagingAPI + " v0.0.0\n" +
		"\t" + stagingAPIServer + " v0.0.0\n" +
		")\n" +
		"\n" +
		"replace " + stagingAPI + " => ./staging/src/" + stagingAPI + "\n" +
		"\n" +
		"replace " + stagingAPIServer + " => ./staging/src/" + stagingAPIServer + "\n",

	"LICENSE": fixtureLicense,
	"NOTICE":  fixtureNotice,

	"staging/src/" + stagingAPI + "/go.mod":                                           "module " + stagingAPI + "\n\ngo 1.26.0\n",
	"staging/src/" + stagingAPI + "/LICENSE":                                          fixtureLicense,
	"staging/src/" + stagingAPI + "/rbac/v1/types.go":                                 stagingAPITypes,
	"staging/src/" + stagingAPIServer + "/go.mod":                                     "module " + stagingAPIServer + "\n\ngo 1.26.0\n",
	"staging/src/" + stagingAPIServer + "/LICENSE":                                    fixtureLicense,
	"staging/src/" + stagingAPIServer + "/pkg/authorization/authorizer/interfaces.go": stagingAuthorizer,

	"plugin/pkg/auth/authorizer/rbac/rbac.go":     upstreamRBAC,
	"pkg/registry/rbac/validation/rule.go":        upstreamValidation,
	"pkg/apis/rbac/v1/doc.go":                     upstreamAPIDoc,
	"pkg/apis/rbac/v1/evaluation_helpers.go":      upstreamAPIHelpers,
	"pkg/apis/rbac/v1/register.go":                upstreamAPIRegister,
	"pkg/apis/rbac/v1/zz_generated.conversion.go": upstreamConversions,
	"pkg/apis/rbac/types.go":                      upstreamInternalAPI,
}

// proxyModules are the staging modules as published versions.
//
// Their contents are the staging directories' contents, because that is what the
// mapping claims: the published version holds the code the source tree staged.
// A fixture where the two disagreed would let a generated module type check
// against something the upstream tree never contained.
var proxyModules = map[string]map[string]string{
	stagingAPI: {
		"go.mod":           "module " + stagingAPI + "\n\ngo 1.26.0\n",
		"LICENSE":          fixtureLicense,
		"rbac/v1/types.go": stagingAPITypes,
	},
	stagingAPIServer: {
		"go.mod":  "module " + stagingAPIServer + "\n\ngo 1.26.0\n",
		"LICENSE": fixtureLicense,
		"pkg/authorization/authorizer/interfaces.go": stagingAuthorizer,
	},
}

// stagingCommits are the staging repository commits each pinned version names.
//
// They are fixed values because the version index records them as evidence that
// a release tag still names what it named before, and a fixture that generated
// them would produce a different report on every run.
var stagingCommits = map[string]string{
	stagingAPI:       "1111111111111111111111111111111111111111",
	stagingAPIServer: "2222222222222222222222222222222222222222",
}

const stagingAPITypes = `package v1

// PolicyRule holds the verbs and resources a subject may act on.
type PolicyRule struct {
	Verbs     []string
	Resources []string
}

// Role is a namespaced collection of rules.
type Role struct {
	Name  string
	Rules []PolicyRule
}
`

const stagingAuthorizer = `package authorizer

import "context"

// Decision is the outcome of one authorization.
type Decision int

const (
	// DecisionDeny refuses the request.
	DecisionDeny Decision = iota
	// DecisionAllow permits it.
	DecisionAllow
	// DecisionNoOpinion defers to the next authorizer.
	DecisionNoOpinion
)

// Attributes describe the request being authorized.
type Attributes interface {
	GetUser() string
	GetVerb() string
}

// Authorizer decides whether a request is permitted.
type Authorizer interface {
	Authorize(ctx context.Context, a Attributes) (Decision, string, error)
}
`

const upstreamRBAC = `package rbac

import (
	"context"

	"k8s.io/kubernetes/pkg/registry/rbac/validation"
	"` + stagingAPIServer + `/pkg/authorization/authorizer"
)

// RBACAuthorizer authorizes requests against RBAC policy.
type RBACAuthorizer struct {
	resolver validation.AuthorizationRuleResolver
}

// New builds an authorizer over the supplied rule resolver.
func New(resolver validation.AuthorizationRuleResolver) *RBACAuthorizer {
	return &RBACAuthorizer{resolver: resolver}
}

// Authorize decides whether the described request is permitted.
func (r *RBACAuthorizer) Authorize(ctx context.Context, a authorizer.Attributes) (authorizer.Decision, string, error) {
	rules, err := r.resolver.RulesFor(a.GetUser())
	if err != nil {
		return authorizer.DecisionNoOpinion, "rule resolution failed", err
	}
	if validation.RuleAllows(rules, a.GetVerb()) {
		return authorizer.DecisionAllow, "allowed by a rule", nil
	}
	return authorizer.DecisionNoOpinion, "no rule allowed the request", nil
}
`

const upstreamValidation = `package validation

import (
	rbachelpers "k8s.io/kubernetes/pkg/apis/rbac/v1"
	rbacv1 "` + stagingAPI + `/rbac/v1"
)

// AuthorizationRuleResolver resolves the rules that apply to a subject.
type AuthorizationRuleResolver interface {
	RulesFor(user string) ([]rbacv1.PolicyRule, error)
}

// RoleGetter retrieves roles by name.
type RoleGetter interface {
	GetRole(name string) (*rbacv1.Role, error)
}

// DefaultRuleResolver resolves rules through a role getter.
type DefaultRuleResolver struct {
	roles RoleGetter
}

// NewDefaultRuleResolver builds a resolver over the supplied getter.
func NewDefaultRuleResolver(roles RoleGetter) *DefaultRuleResolver {
	return &DefaultRuleResolver{roles: roles}
}

// RulesFor returns the rules bound to the subject.
func (r *DefaultRuleResolver) RulesFor(user string) ([]rbacv1.PolicyRule, error) {
	role, err := r.roles.GetRole(user)
	if err != nil {
		return nil, err
	}
	return role.Rules, nil
}

// RuleAllows reports whether any rule covers the verb.
func RuleAllows(rules []rbacv1.PolicyRule, verb string) bool {
	for i := range rules {
		if rbachelpers.VerbMatches(&rules[i], verb) {
			return true
		}
	}
	return false
}
`

const upstreamAPIDoc = `// Package v1 holds helpers for the versioned RBAC API.
//
// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac
// +k8s:conversion-gen-external-types=` + stagingAPI + `/rbac/v1
// +groupName=rbac.authorization.k8s.io
package v1
`

// upstreamConversions is the generated conversion file, and the second of the
// two files the profile prunes.
//
// It is what makes the type policy provable rather than merely plausible.
// Upstream generated these functions, so the two declarations are mechanically
// known to hold the same fields in the same shape, and the analysis reads that
// rather than comparing type names and hoping. Its body is exactly what
// conversion-gen emits: assignments and a nil return, with no statement that
// required a decision.
const upstreamConversions = `//go:build !ignore_autogenerated

// Code generated by conversion-gen. DO NOT EDIT.

package v1

import (
	rbac "k8s.io/kubernetes/pkg/apis/rbac"
	rbacv1 "` + stagingAPI + `/rbac/v1"
)

// Convert_v1_PolicyRule_To_rbac_PolicyRule is an autogenerated conversion function.
func Convert_v1_PolicyRule_To_rbac_PolicyRule(in *rbacv1.PolicyRule, out *rbac.PolicyRule) error {
	out.Verbs = in.Verbs
	out.Resources = in.Resources
	return nil
}

// Convert_rbac_PolicyRule_To_v1_PolicyRule is an autogenerated conversion function.
func Convert_rbac_PolicyRule_To_v1_PolicyRule(in *rbac.PolicyRule, out *rbacv1.PolicyRule) error {
	out.Verbs = in.Verbs
	out.Resources = in.Resources
	return nil
}
`

const upstreamAPIHelpers = `package v1

import rbacv1 "` + stagingAPI + `/rbac/v1"

// VerbMatches reports whether the rule covers the requested verb.
func VerbMatches(rule *rbacv1.PolicyRule, requestedVerb string) bool {
	for _, verb := range rule.Verbs {
		if verb == "*" || verb == requestedVerb {
			return true
		}
	}
	return false
}
`

// upstreamAPIRegister is the one file the profile prunes.
//
// It exists only to import the unversioned internal API package, which is what
// puts that package in the closure. Removing it is what the type policy is asked
// to prove is safe: the code that survives already speaks the published API.
const upstreamAPIRegister = `package v1

import "k8s.io/kubernetes/pkg/apis/rbac"

// internalRule keeps the unversioned API package reachable from this one.
var internalRule = rbac.PolicyRule{}
`

const upstreamInternalAPI = `package rbac

// PolicyRule is the unversioned form of a policy rule.
type PolicyRule struct {
	Verbs     []string
	Resources []string
}
`

// conversionsWithoutFunctions is the generated conversion file stripped of the
// functions that carry the proof.
//
// The file still exists, so the profile's prune entry still matches, and the
// closure is unchanged. What is gone is the mechanical evidence that the two
// declarations hold the same fields, which is what the type policy refuses on.
const conversionsWithoutFunctions = `//go:build !ignore_autogenerated

// Code generated by conversion-gen. DO NOT EDIT.

package v1
`

// fixtureLicense carries the phrases the licence verifier matches, because the
// generated files quote the obligations of a particular licence by section
// number and a record naming the wrong one would misstate what the module owes.
const fixtureLicense = `                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
`

const fixtureNotice = `Kubernetes
Copyright 2014 The Kubernetes Authors.

This product includes software developed at
The Kubernetes Authors (https://kubernetes.io/).
`

// upstream is one materialized fixture repository.
type upstream struct {
	repo   *testsupport.Repo
	commit string
}

// url renders the remote a generation is pointed at.
func (u *upstream) url() string { return "file://" + u.repo.Dir }

// newUpstreamWith builds, commits, and tags the fixture source repository, with
// some files replaced.
//
// Overriding rather than omitting is deliberate. The profile's prune list fails
// closed on a path that is not there, so a fixture that deleted a file would
// refuse during extraction and never reach the phase a test was aiming at.
// Replacing the contents keeps the tree's shape and changes only the evidence.
func newUpstreamWith(ctx context.Context, t *testing.T, overrides map[string]string) *upstream {
	t.Helper()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    fixtureBranch,
		UserName:  "Fixture Author",
		UserEmail: "fixture@example.test",
	})
	// The source cache is a blobless partial clone, and opening it audits that
	// the filter actually took effect. A fixture remote that ignored the filter
	// would fail that audit rather than this one.
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")

	files := make(map[string]string, len(upstreamFiles))
	maps.Copy(files, upstreamFiles)
	maps.Copy(files, overrides)

	paths := make([]string, 0, len(files))
	for path, contents := range files {
		repo.WriteFile(t, path, contents)
		paths = append(paths, path)
	}
	slices.Sort(paths)

	commit := repo.Commit(ctx, t, "feat: add the rbac authorizer\n", gitcli.CommitOptions{}, paths...)
	// The tagger date is a literal rather than a clock, so two runs over this
	// fixture produce the same tag object and therefore the same commit.
	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name:    fixtureTag,
		Commit:  commit,
		Message: "Kubernetes " + fixtureTag + "\n",
		Tagger: gitcli.Signature{
			Name:  "Fixture Author",
			Email: "fixture@example.test",
			Date:  "2026-01-02T03:04:05Z",
		},
	}); err != nil {
		t.Fatalf("tag fixture: %v", err)
	}
	return &upstream{repo: repo, commit: commit}
}

// newProxy writes the staging modules into a directory the go command can serve
// as a module proxy.
//
// A file proxy is used rather than a network one because these tests must run
// offline and must not depend on anything published. It is a real proxy rather
// than a replace directive on purpose: the generated module carries no
// replacements, so resolving its staging requirements the way a consumer would
// is the only thing that proves the generated go.mod is usable.
func newProxy(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "proxy")
	for modulePath, files := range proxyModules {
		writeProxyModule(t, root, modulePath, fixtureStagingTag, files)
	}
	return root
}

// writeProxyModule lays out one module version in the proxy's directory format.
func writeProxyModule(t *testing.T, root, modulePath, version string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(modulePath), "@v")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("proxy directory: %v", err)
	}

	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("proxy file %s: %v", name, err)
		}
	}
	write("list", version+"\n")
	// The timestamp is a literal so the proxy is a pure function of the fixture.
	write(version+".info", `{"Version":"`+version+`","Time":"2026-01-02T03:04:05Z"}`)
	write(version+".mod", files["go.mod"])

	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	prefix := modulePath + "@" + version + "/"
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry, err := archive.Create(prefix + name)
		if err != nil {
			t.Fatalf("proxy zip entry %s: %v", name, err)
		}
		if _, err := entry.Write([]byte(files[name])); err != nil {
			t.Fatalf("proxy zip write %s: %v", name, err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("proxy zip: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, version+".zip"), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("proxy zip file: %v", err)
	}
}

// writeVersionIndex pre-populates the staging version index for one source
// commit.
//
// This is the cache path a replay uses rather than a way around the resolver.
// Resolving a release version asks the go command which tag a version came from
// and which commit that tag names, and a file proxy carries no version control
// origin at all, so the resolution itself needs a real proxy and a real
// repository. What these tests exercise instead is the path every run after the
// first one takes, which is the one the pins actually reach the module through.
func writeVersionIndex(ctx context.Context, t *testing.T, path, commit string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("version index directory: %v", err)
	}
	store, err := gomodmap.NewStore(path)
	if err != nil {
		t.Fatalf("version index: %v", err)
	}
	index := gomodmap.NewIndex()
	modules := make([]gomodmap.ModuleVersion, 0, len(stagingCommits))
	for _, modulePath := range []string{stagingAPI, stagingAPIServer} {
		modules = append(modules, gomodmap.ModuleVersion{
			Path:    modulePath,
			Version: fixtureStagingTag,
			Commit:  stagingCommits[modulePath],
		})
	}
	if err := index.Put(gomodmap.Entry{Source: commit, Tag: fixtureTag, Modules: modules}); err != nil {
		t.Fatalf("version index entry: %v", err)
	}
	if err := store.Save(ctx, index); err != nil {
		t.Fatalf("version index save: %v", err)
	}
}

// removeAllForced removes a tree the go command made read-only.
//
// The module cache is written without write permission so a build cannot mutate
// a downloaded module in place, which also means an ordinary removal cannot
// unlink it. Restoring write permission on the way down is what lets a test
// clean up after itself.
func removeAllForced(root string) error {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("relax %s: %w", root, err)
	}
	return os.RemoveAll(root)
}

// goEnvironmentPrefix is the environment variable name a test sets so the go
// command does not consult the checksum database.
//
// The runner deliberately leaves GOSUMDB at its default and empties every
// exemption list, which is the right policy for a run resolving public modules.
// A fixture proxy publishes modules no checksum database has ever seen, so the
// database is switched off for these tests through the one route the runner
// leaves open, which is inheriting the variable from the process.
const goSumDBVariable = "GOSUMDB"

// stagingPaths renders the staging module paths the fixture provides, sorted.
func stagingPaths() []string {
	paths := []string{stagingAPI, stagingAPIServer}
	slices.Sort(paths)
	return paths
}

// relativeTo renders a path below a root for a readable assertion failure.
func relativeTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// containsAll reports the first wanted value missing from got.
func containsAll(got, want []string) (string, bool) {
	for _, value := range want {
		if !slices.Contains(got, value) {
			return value, false
		}
	}
	return "", true
}

// joinLines renders a list for an assertion failure.
func joinLines(values []string) string { return strings.Join(values, "\n  ") }
