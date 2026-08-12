package facade_test

import (
	"errors"
	"testing"

	"github.com/enj/soapbox/tools/internal/facade"
)

// TestSpecRefusesUnusableProfiles covers everything a specification can get
// wrong on its own.
//
// All of it is checked before a module is loaded, because loading is the
// expensive half of generation and a profile mistake should not cost one. The
// cases are the mistakes a profile author can actually make: a name that is not
// a Go identifier, two exports competing for one name, a source outside the
// upstream module, an assertion about something the facade does not publish,
// and an assertion against a copy of an interface rather than the real one.
func TestSpecRefusesUnusableProfiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*facade.Spec)
		want   error
	}{
		{
			name:   "no module path",
			mutate: func(s *facade.Spec) { s.ModulePath = "" },
			want:   facade.ErrSpec,
		},
		{
			name:   "internal prefix without an internal element",
			mutate: func(s *facade.Spec) { s.InternalPrefix = "vendor/kk" },
			want:   facade.ErrSpec,
		},
		{
			name:   "exported package name",
			mutate: func(s *facade.Spec) { s.Package = "RBACAuthorizer" },
			want:   facade.ErrSpec,
		},
		{
			name:   "facade file outside the module root",
			mutate: func(s *facade.Spec) { s.File = "pkg/authorizer.go" },
			want:   facade.ErrSpec,
		},
		{
			name:   "facade file that is a test file",
			mutate: func(s *facade.Spec) { s.File = "authorizer_test.go" },
			want:   facade.ErrSpec,
		},
		{
			name:   "facade and assertions in one file",
			mutate: func(s *facade.Spec) { s.AssertionsFile = s.File },
			want:   facade.ErrSpec,
		},
		{
			name:   "no exports",
			mutate: func(s *facade.Spec) { s.Exports = nil },
			want:   facade.ErrSpec,
		},
		{
			name: "unexported facade name",
			mutate: func(s *facade.Spec) {
				s.Exports = append(s.Exports, facade.Export{Name: "newThing", Kind: facade.KindFunc, Source: rbacPackage + ".New"})
			},
			want: facade.ErrSpec,
		},
		{
			name: "source that is not a qualified symbol",
			mutate: func(s *facade.Spec) {
				s.Exports = append(s.Exports, facade.Export{Name: "Thing", Kind: facade.KindFunc, Source: "NoPackage"})
			},
			want: facade.ErrSpec,
		},
		{
			name: "source outside the upstream module",
			mutate: func(s *facade.Spec) {
				s.Exports = append(s.Exports, facade.Export{Name: "Thing", Kind: facade.KindFunc, Source: "k8s.io/apiserver/pkg/x.New"})
			},
			want: facade.ErrSpec,
		},
		{
			name: "two exports competing for one name",
			mutate: func(s *facade.Spec) {
				s.Exports = append(s.Exports, facade.Export{Name: "RoleGetter", Kind: facade.KindType, Source: rbacPackage + ".RoleGetter"})
			},
			want: facade.ErrCollision,
		},
		{
			name: "exported variable",
			mutate: func(s *facade.Spec) {
				s.Exports = append(s.Exports, facade.Export{Name: "Registry", Kind: facade.KindVar, Source: rbacPackage + ".Registry"})
			},
			want: facade.ErrMutableVar,
		},
		{
			name: "assertion about a name the facade does not publish",
			mutate: func(s *facade.Spec) {
				s.Assertions = append(s.Assertions, facade.Assertion{Type: "Missing", Interface: authorizerPkg + ".Authorizer"})
			},
			want: facade.ErrSpec,
		},
		{
			name: "assertion about something that is not a type",
			mutate: func(s *facade.Spec) {
				s.Assertions = append(s.Assertions, facade.Assertion{Type: "New", Interface: authorizerPkg + ".Authorizer"})
			},
			want: facade.ErrSpec,
		},
		{
			name: "assertion against an interface inside the generated module",
			mutate: func(s *facade.Spec) {
				s.Assertions = append(s.Assertions, facade.Assertion{
					Type:      "RBACAuthorizer",
					Interface: generatedModule + "/" + internalPrefix + "/plugin/pkg/auth/authorizer/rbac.SubjectLocator",
				})
			},
			want: facade.ErrSpec,
		},
		{
			name: "one assertion declared twice",
			mutate: func(s *facade.Spec) {
				s.Assertions = append(s.Assertions, facade.Assertion{
					Type: "RBACAuthorizer", Pointer: true, Interface: authorizerPkg + ".Authorizer",
				})
			},
			want: facade.ErrSpec,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			spec := rbacSpec()
			test.mutate(&spec)
			// The directory and environment are deliberately unusable: a
			// specification failure has to be reported without loading, so a
			// load error here would mean the check ran too late.
			_, err := facade.Generate(t.Context(), facade.Options{
				Dir:  "/nonexistent",
				Env:  []string{"HOME=/nonexistent"},
				Spec: spec,
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("generate error is %v, want %v", err, test.want)
			}
			if errors.Is(err, facade.ErrLoad) {
				t.Errorf("specification error %v was only found after loading the module", err)
			}
		})
	}
}

// TestParseKind maps the profile spelling onto the generator's kinds, which is
// how configuration converts without this package depending on one schema.
func TestParseKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []facade.Kind{facade.KindType, facade.KindInterface, facade.KindFunc, facade.KindConst, facade.KindVar} {
		parsed, err := facade.ParseKind(kind.String())
		if err != nil {
			t.Errorf("ParseKind(%q): %v", kind.String(), err)
			continue
		}
		if parsed != kind {
			t.Errorf("ParseKind(%q) = %v, want %v", kind.String(), parsed, kind)
		}
	}
	if _, err := facade.ParseKind("struct"); !errors.Is(err, facade.ErrSpec) {
		t.Errorf("ParseKind(\"struct\") error is %v, want ErrSpec", err)
	}
}
