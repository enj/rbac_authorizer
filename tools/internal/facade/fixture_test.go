package facade_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enj/soapbox/tools/internal/facade"
	"github.com/enj/soapbox/tools/internal/gocli"
)

// The fixture modules mirror the shape of the real extraction rather than a
// convenient simplification, because every interesting property of the
// generator is a property of that shape: two relocated packages that export the
// same four names, an authorizer whose method set has to satisfy interfaces
// belonging to a different module, and API types that must keep the identity
// their own module gave them.
//
// The dependencies are real modules resolved through filesystem replacements
// rather than stubs compiled into the same module. That distinction is the
// whole point of the external identity checks: a type is external because it
// belongs to another module, and a fake that lives in the module under test
// would satisfy every assertion while proving nothing.
const (
	generatedModule = "monis.app/kk/rbac_authorizer"
	sourcePrefix    = "k8s.io/kubernetes"
	internalPrefix  = "internal/kk"
	rbacPackage     = sourcePrefix + "/plugin/pkg/auth/authorizer/rbac"
	validationPkg   = sourcePrefix + "/pkg/registry/rbac/validation"
	authorizerPkg   = "example.com/apiserver/pkg/authorization/authorizer"
)

// apiModule is a stand in for k8s.io/api: the API types the extracted code
// passes around, which must keep their own module's identity in the published
// signatures.
var apiModule = map[string]string{
	"go.mod": "module example.com/api\n\ngo 1.26.0\n",
	"rbac/v1/types.go": `package v1

type PolicyRule struct {
	Verbs     []string
	APIGroups []string
	Resources []string
}

type RoleRef struct {
	APIGroup string
	Kind     string
	Name     string
}

type Subject struct {
	Kind      string
	Name      string
	Namespace string
}

type Role struct {
	Name  string
	Rules []PolicyRule
}

type RoleBinding struct {
	Name     string
	Subjects []Subject
	RoleRef  RoleRef
}

type ClusterRole struct {
	Name  string
	Rules []PolicyRule
}

type ClusterRoleBinding struct {
	Name     string
	Subjects []Subject
	RoleRef  RoleRef
}
`,
}

// apiserverModule is a stand in for k8s.io/apiserver: the interfaces the
// generated authorizer has to implement for the wider ecosystem to accept it.
var apiserverModule = map[string]string{
	"go.mod": "module example.com/apiserver\n\ngo 1.26.0\n",
	"pkg/authorization/authorizer/interfaces.go": `package authorizer

import (
	"context"

	"example.com/apiserver/pkg/internal/private"
)

type Decision int

const (
	DecisionDeny Decision = iota
	DecisionAllow
	DecisionNoOpinion
)

type Attributes interface {
	GetUser() string
	GetVerb() string
	GetNamespace() string
	GetResource() string
	IsResourceRequest() bool
}

type Authorizer interface {
	Authorize(ctx context.Context, a Attributes) (authorized Decision, reason string, err error)
}

type ResourceRuleInfo interface {
	GetVerbs() []string
	GetAPIGroups() []string
	GetResources() []string
}

type NonResourceRuleInfo interface {
	GetVerbs() []string
	GetNonResourceURLs() []string
}

type RuleResolver interface {
	RulesFor(ctx context.Context, user string, namespace string) ([]ResourceRuleInfo, []NonResourceRuleInfo, bool, error)
}

// Extended exposes a type from this module's own internal package, which is a
// thing real dependencies do and which a consumer of a different module cannot
// name.
type Extended interface {
	Handle() *private.Handle
}

// Handle is the spelling this module gives its own internal type, which every
// importer may use even though the package it is declared in is unreachable.
type Handle = private.Handle
`,
	"pkg/internal/private/private.go": `package private

type Handle struct {
	ID string
}
`,
}

// generatedModuleFiles is the relocated tree of the module under test.
var generatedModuleFiles = map[string]string{
	"go.mod": `module ` + generatedModule + `

go 1.26.0

require (
	example.com/api v0.0.0
	example.com/apiserver v0.0.0
)

replace example.com/api => ../api

replace example.com/apiserver => ../apiserver
`,
	internalPrefix + "/pkg/registry/rbac/validation/rule.go": `package validation

import (
	"context"

	rbacv1 "example.com/api/rbac/v1"
)

// PolicyRuleLimit is a constant the facade forwards.
const PolicyRuleLimit = 64

// Decision is a defined type over a predeclared one, whose underlying identity
// is the only thing that records a change to it.
type Decision int

type RoleGetter interface {
	GetRole(ctx context.Context, namespace, name string) (*rbacv1.Role, error)
}

type RoleBindingLister interface {
	ListRoleBindings(ctx context.Context, namespace string) ([]*rbacv1.RoleBinding, error)
}

type ClusterRoleGetter interface {
	GetClusterRole(ctx context.Context, name string) (*rbacv1.ClusterRole, error)
}

type ClusterRoleBindingLister interface {
	ListClusterRoleBindings(ctx context.Context) ([]*rbacv1.ClusterRoleBinding, error)
}

type AuthorizationRuleResolver interface {
	GetRoleReferenceRules(ctx context.Context, roleRef rbacv1.RoleRef, namespace string) ([]rbacv1.PolicyRule, error)
	RulesFor(ctx context.Context, user string, namespace string) ([]rbacv1.PolicyRule, error)
}

type DefaultRuleResolver struct {
	roleGetter               RoleGetter
	roleBindingLister        RoleBindingLister
	clusterRoleGetter        ClusterRoleGetter
	clusterRoleBindingLister ClusterRoleBindingLister
}

func NewDefaultRuleResolver(roleGetter RoleGetter, roleBindingLister RoleBindingLister, clusterRoleGetter ClusterRoleGetter, clusterRoleBindingLister ClusterRoleBindingLister) *DefaultRuleResolver {
	return &DefaultRuleResolver{roleGetter, roleBindingLister, clusterRoleGetter, clusterRoleBindingLister}
}

func (r *DefaultRuleResolver) GetRoleReferenceRules(ctx context.Context, roleRef rbacv1.RoleRef, namespace string) ([]rbacv1.PolicyRule, error) {
	return nil, nil
}

func (r *DefaultRuleResolver) RulesFor(ctx context.Context, user string, namespace string) ([]rbacv1.PolicyRule, error) {
	return nil, nil
}
`,
	internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go": `package rbac

import (
	"context"

	rbacv1 "example.com/api/rbac/v1"
	"example.com/apiserver/pkg/authorization/authorizer"

	"` + generatedModule + `/` + internalPrefix + `/pkg/registry/rbac/validation"
)

type RequestToRuleMapper interface {
	RulesFor(ctx context.Context, subject string, namespace string) ([]rbacv1.PolicyRule, error)
}

type RoleToRuleMapper interface {
	GetRoleReferenceRules(ctx context.Context, roleRef rbacv1.RoleRef, namespace string) ([]rbacv1.PolicyRule, error)
}

type SubjectLocator interface {
	AllowedSubjects(ctx context.Context, attributes authorizer.Attributes) ([]rbacv1.Subject, error)
}

type RBACAuthorizer struct {
	authorizationRuleResolver validation.AuthorizationRuleResolver
}

func New(roles validation.RoleGetter, roleBindings validation.RoleBindingLister, clusterRoles validation.ClusterRoleGetter, clusterRoleBindings validation.ClusterRoleBindingLister) *RBACAuthorizer {
	return &RBACAuthorizer{authorizationRuleResolver: validation.NewDefaultRuleResolver(roles, roleBindings, clusterRoles, clusterRoleBindings)}
}

func (r *RBACAuthorizer) Authorize(ctx context.Context, requestAttributes authorizer.Attributes) (authorizer.Decision, string, error) {
	return authorizer.DecisionNoOpinion, "", nil
}

func (r *RBACAuthorizer) RulesFor(ctx context.Context, user string, namespace string) ([]authorizer.ResourceRuleInfo, []authorizer.NonResourceRuleInfo, bool, error) {
	return nil, nil, false, nil
}

type SubjectAccessEvaluator struct {
	superUser string
}

func NewSubjectAccessEvaluator(roles validation.RoleGetter, roleBindings validation.RoleBindingLister, clusterRoles validation.ClusterRoleGetter, clusterRoleBindings validation.ClusterRoleBindingLister, superUser string) *SubjectAccessEvaluator {
	return &SubjectAccessEvaluator{superUser: superUser}
}

func (e *SubjectAccessEvaluator) AllowedSubjects(ctx context.Context, attributes authorizer.Attributes) ([]rbacv1.Subject, error) {
	return nil, nil
}

// RulesAllow is variadic, which a forwarding declaration has to preserve
// exactly or every existing call site stops compiling.
func RulesAllow(requestAttributes authorizer.Attributes, rules ...rbacv1.PolicyRule) bool {
	return false
}

func RuleAllows(requestAttributes authorizer.Attributes, rule *rbacv1.PolicyRule) bool {
	return false
}

// These four adapters collide by name with the validation interfaces they
// adapt, which is why the profile gives them explicit facade names.
type RoleGetter struct {
	Lister func(namespace string) ([]*rbacv1.Role, error)
}

type RoleBindingLister struct {
	Lister func(namespace string) ([]*rbacv1.RoleBinding, error)
}

type ClusterRoleGetter struct {
	Lister func() ([]*rbacv1.ClusterRole, error)
}

type ClusterRoleBindingLister struct {
	Lister func() ([]*rbacv1.ClusterRoleBinding, error)
}
`,
}

// rbacSpec is the published surface of the fixture, which mirrors the profile's
// own facade contract including the four explicit collision renames.
func rbacSpec() facade.Spec {
	spec := facade.Spec{
		ModulePath:     generatedModule,
		SourcePrefix:   sourcePrefix,
		InternalPrefix: internalPrefix,
		Package:        "rbacauthorizer",
		File:           "authorizer.go",
		AssertionsFile: "zz_generated_assertions.go",
		Assertions: []facade.Assertion{
			{Type: "RBACAuthorizer", Pointer: true, Interface: authorizerPkg + ".Authorizer"},
			{Type: "RBACAuthorizer", Pointer: true, Interface: authorizerPkg + ".RuleResolver"},
		},
	}
	for _, entry := range []struct {
		name   string
		kind   facade.Kind
		source string
	}{
		{"New", facade.KindFunc, rbacPackage + ".New"},
		{"RBACAuthorizer", facade.KindType, rbacPackage + ".RBACAuthorizer"},
		{"RequestToRuleMapper", facade.KindInterface, rbacPackage + ".RequestToRuleMapper"},
		{"RoleToRuleMapper", facade.KindInterface, rbacPackage + ".RoleToRuleMapper"},
		{"SubjectLocator", facade.KindInterface, rbacPackage + ".SubjectLocator"},
		{"SubjectAccessEvaluator", facade.KindType, rbacPackage + ".SubjectAccessEvaluator"},
		{"NewSubjectAccessEvaluator", facade.KindFunc, rbacPackage + ".NewSubjectAccessEvaluator"},
		{"RulesAllow", facade.KindFunc, rbacPackage + ".RulesAllow"},
		{"RuleAllows", facade.KindFunc, rbacPackage + ".RuleAllows"},
		{"AuthorizationRuleResolver", facade.KindInterface, validationPkg + ".AuthorizationRuleResolver"},
		{"DefaultRuleResolver", facade.KindType, validationPkg + ".DefaultRuleResolver"},
		{"NewDefaultRuleResolver", facade.KindFunc, validationPkg + ".NewDefaultRuleResolver"},
		{"RoleGetter", facade.KindInterface, validationPkg + ".RoleGetter"},
		{"RoleBindingLister", facade.KindInterface, validationPkg + ".RoleBindingLister"},
		{"ClusterRoleGetter", facade.KindInterface, validationPkg + ".ClusterRoleGetter"},
		{"ClusterRoleBindingLister", facade.KindInterface, validationPkg + ".ClusterRoleBindingLister"},
		{"PolicyRuleLimit", facade.KindConst, validationPkg + ".PolicyRuleLimit"},
		{"Decision", facade.KindType, validationPkg + ".Decision"},
		// The four adapters, under explicit names, because their upstream names
		// are already taken by the interfaces above.
		{"RoleGetterFromLister", facade.KindType, rbacPackage + ".RoleGetter"},
		{"RoleBindingListerFromLister", facade.KindType, rbacPackage + ".RoleBindingLister"},
		{"ClusterRoleGetterFromLister", facade.KindType, rbacPackage + ".ClusterRoleGetter"},
		{"ClusterRoleBindingListerFromLister", facade.KindType, rbacPackage + ".ClusterRoleBindingLister"},
	} {
		spec.Exports = append(spec.Exports, facade.Export{Name: entry.name, Kind: entry.kind, Source: entry.source})
	}
	return spec
}

// newRBACFixture writes the three modules and returns the generated module's
// root directory.
func newRBACFixture(t *testing.T) string {
	t.Helper()
	return newFixture(t, generatedModuleFiles)
}

// newFixture writes the two dependency modules plus the given generated module
// tree, and returns the generated module's root.
func newFixture(t *testing.T, generated map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeModule(t, filepath.Join(root, "api"), apiModule)
	writeModule(t, filepath.Join(root, "apiserver"), apiserverModule)
	generatedRoot := filepath.Join(root, "gen")
	writeModule(t, generatedRoot, generated)
	return generatedRoot
}

// writeModule writes one module tree.
func writeModule(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// goEnvironment builds the complete environment the loader runs the Go
// toolchain under.
//
// The values come from the Go command itself, through the engine's own runner,
// rather than from the process environment. That is what the loader requires of
// every caller: an explicit environment, so the code the public API is
// generated from does not depend on the shell the run started in. Downloads are
// off, so the fixture proves it resolves its dependencies through the
// filesystem replacements and nothing else.
func goEnvironment(t *testing.T, dir string) []string {
	t.Helper()
	home := newIsolatedHome(t)
	isolation := []string{"HOME=" + home}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			isolation = append(isolation, name+"="+value)
		}
	}
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH"},
		Isolation: isolation,
		Proxy:     gocli.ProxyOff,
	})
	if err != nil {
		t.Fatalf("go runner: %v", err)
	}
	values, err := runner.Env(t.Context(), "GOROOT", "GOCACHE", "GOMODCACHE", "GOPATH")
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + t.TempDir(),
		"GOROOT=" + values["GOROOT"],
		"GOCACHE=" + values["GOCACHE"],
		"GOMODCACHE=" + values["GOMODCACHE"],
		"GOPATH=" + values["GOPATH"],
		"GOPROXY=" + gocli.ProxyOff,
		"GOFLAGS=",
		"GOWORK=off",
		"GOENV=off",
		"GOTOOLCHAIN=local",
		"LC_ALL=C",
	}
	return env
}

// newIsolatedHome returns a throwaway HOME with telemetry turned off, so the go
// command does not race the temporary directory cleanup writing counter files.
func newIsolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".config", "go", "telemetry", "mode")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create telemetry directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("off\n"), 0o600); err != nil {
		t.Fatalf("write telemetry mode: %v", err)
	}
	return home
}

// generate runs the generator against a fixture and fails the test on error.
func generate(t *testing.T, dir string, spec facade.Spec) facade.Result {
	t.Helper()
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return result
}

// fileNamed reports the generated file at a path.
func fileNamed(t *testing.T, result facade.Result, name string) string {
	t.Helper()
	for _, file := range result.Files {
		if file.Path == name {
			return string(file.Contents)
		}
	}
	t.Fatalf("generated files do not include %s", name)
	return ""
}
