package soapbox_test

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	soapbox "github.com/enj/soapbox/tools"
	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/doctor"
)

func TestMainRunsCommands(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{name: "version", args: []string{"version"}, wantCode: soapbox.ExitOK, wantOut: "soapbox " + soapbox.Version},
		{name: "help", args: []string{"help"}, wantCode: soapbox.ExitOK, wantOut: "soapbox <command> [flags]"},
		{name: "unknown command", args: []string{"nope"}, wantCode: soapbox.ExitUsage},
		{name: "no command", args: nil, wantCode: soapbox.ExitUsage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := soapbox.Run(t.Context(), test.args, &stdout, &stderr, t.TempDir())
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, test.wantCode, &stdout, &stderr)
			}
			if test.wantOut != "" && !strings.Contains(stdout.String(), test.wantOut) {
				t.Fatalf("stdout %q does not contain %q", &stdout, test.wantOut)
			}
		})
	}
}

func TestRunReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := soapbox.Run(ctx, []string{"doctor"}, &stdout, &stderr, t.TempDir()); code != soapbox.ExitCanceled {
		t.Fatalf("exit code = %d, want %d", code, soapbox.ExitCanceled)
	}
}

// TestEngineNeverExecutesAShell enforces the hard constraint that maintained
// executable logic is Go. Commands are built as explicit argument vectors, so no
// source file may hand a command line to a shell interpreter. golangci-lint
// enforces the same rule through forbidigo and gosec, and this test keeps the
// invariant checked by go test alone.
func TestEngineNeverExecutesAShell(t *testing.T) {
	shells := map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
		"/bin/sh": true, "/bin/bash": true, "/usr/bin/sh": true, "/usr/bin/bash": true,
		"cmd": true, "cmd.exe": true, "powershell": true, "pwsh": true,
	}
	forEachCall(t, func(t *testing.T, path string, fset *token.FileSet, call *ast.CallExpr) {
		name := calleeName(call)
		if !strings.HasPrefix(name, "exec.Command") && !strings.HasPrefix(name, "exec.LookPath") {
			return
		}
		for _, arg := range call.Args {
			value, ok := stringLiteral(arg)
			if !ok {
				continue
			}
			if shells[value] || value == "-c" {
				t.Errorf("%s: %s passes %q to a subprocess, engine logic never composes shell commands",
					position(fset, arg), name, value)
			}
		}
	})
}

// TestEngineNeverDisablesTLSVerification forbids turning off certificate
// verification anywhere in the engine.
func TestEngineNeverDisablesTLSVerification(t *testing.T) {
	forEachFile(t, func(t *testing.T, path string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.KeyValueExpr:
				if ident, ok := typed.Key.(*ast.Ident); ok && ident.Name == "InsecureSkipVerify" {
					if literal, ok := typed.Value.(*ast.Ident); !ok || literal.Name != "false" {
						t.Errorf("%s: InsecureSkipVerify must never be enabled", position(fset, typed))
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range typed.Lhs {
					if selector, ok := lhs.(*ast.SelectorExpr); ok && selector.Sel.Name == "InsecureSkipVerify" {
						t.Errorf("%s: InsecureSkipVerify must never be assigned", position(fset, typed))
					}
				}
			}
			return true
		})
	})
}

// TestSubprocessExecutionStaysInsideItsBoundary keeps os/exec confined to the
// packages that are allowed to start processes.
func TestSubprocessExecutionStaysInsideItsBoundary(t *testing.T) {
	allowed := map[string]bool{
		"internal/gitcli": true,
		"internal/gocli":  true,
		"internal/doctor": true,
	}
	forEachFile(t, func(t *testing.T, path string, fset *token.FileSet, file *ast.File) {
		for _, spec := range file.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil || value != "os/exec" {
				continue
			}
			if !allowed[filepath.ToSlash(filepath.Dir(path))] {
				t.Errorf("%s: os/exec may only be imported by the git, go, and doctor boundaries", position(fset, spec))
			}
		}
	})
}

// TestEveryPackageIsDocumented keeps the engine navigable.
func TestEveryPackageIsDocumented(t *testing.T) {
	documented := make(map[string]bool)
	packages := make(map[string]bool)
	forEachFile(t, func(t *testing.T, path string, _ *token.FileSet, file *ast.File) {
		dir := filepath.ToSlash(filepath.Dir(path))
		if strings.HasSuffix(file.Name.Name, "_test") {
			return
		}
		packages[dir] = true
		if file.Doc != nil {
			documented[dir] = true
		}
	})
	for dir := range packages {
		if !documented[dir] {
			t.Errorf("package %s has no package comment", dir)
		}
	}
}

// TestPinnedToolchainAgreesEverywhere keeps the three places that name the Go
// toolchain in agreement: the shared constant the engine reports, the module
// that builds it, and the profile that pins generated formatting.
func TestPinnedToolchainAgreesEverywhere(t *testing.T) {
	goMod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	directives := map[string]string{}
	for line := range strings.SplitSeq(string(goMod), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[0] == "toolchain" || fields[0] == "go") {
			directives[fields[0]] = fields[1]
		}
	}
	if got := directives["toolchain"]; got != buildinfo.Toolchain {
		t.Errorf("go.mod toolchain = %q, want %q", got, buildinfo.Toolchain)
	}
	if got := directives["go"]; got != buildinfo.GoDirective {
		t.Errorf("go.mod go directive = %q, want %q", got, buildinfo.GoDirective)
	}

	path := filepath.Join("..", config.DefaultFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s is not present in this checkout", path)
	}
	cfg, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatalf("load repository profile: %v", err)
	}
	if cfg.Determinism.Toolchain != buildinfo.Toolchain {
		t.Errorf("%s toolchain = %q, want %q", config.DefaultFileName, cfg.Determinism.Toolchain, buildinfo.Toolchain)
	}
	if doctor.SoapboxPolicy().Toolchain != buildinfo.Toolchain {
		t.Errorf("doctor policy toolchain = %q, want %q", doctor.SoapboxPolicy().Toolchain, buildinfo.Toolchain)
	}
	if soapbox.Version != buildinfo.Version {
		t.Errorf("engine version = %q, want %q", soapbox.Version, buildinfo.Version)
	}
}

// forEachFile parses every Go file in the engine module.
func forEachFile(t *testing.T, visit func(t *testing.T, path string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	root := "."
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") && entry.Name() != "." {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		visit(t, path, fset, file)
		return nil
	})
	if err != nil {
		t.Fatalf("walk engine sources: %v", err)
	}
}

// forEachCall visits every call expression in the engine module.
func forEachCall(t *testing.T, visit func(t *testing.T, path string, fset *token.FileSet, call *ast.CallExpr)) {
	t.Helper()
	forEachFile(t, func(t *testing.T, path string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				visit(t, path, fset, call)
			}
			return true
		})
	})
}

// calleeName renders the callee of a call expression as written in source.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if ident, ok := fun.X.(*ast.Ident); ok {
			return ident.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	default:
		return ""
	}
}

// stringLiteral reports the value of a string literal expression.
func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// position renders a source position.
func position(fset *token.FileSet, node ast.Node) string {
	return fset.Position(node.Pos()).String()
}
