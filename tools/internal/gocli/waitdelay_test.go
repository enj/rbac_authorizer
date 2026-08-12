package gocli_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/gocli"
)

// standInHold is how long the grandchild keeps the output pipe open. It has to
// be well past the runner's own delay for the assertion to mean anything, and
// short enough that a process left behind by a failing test does not outlive the
// run by much.
const standInHold = 30 * time.Second

// standInSource is a stand-in for the go command that exits immediately and
// leaves a child holding the standard output it inherited.
//
// The nanosecond literal is filled in from standInHold so the program and the
// assertion cannot drift apart.
const standInSource = `package main

import (
	"os"
	"os/exec"
	"time"
)

// hold marks the child invocation that keeps the inherited pipe open.
const hold = "soapbox-hold"

func main() {
	for _, arg := range os.Args[1:] {
		if arg == hold {
			time.Sleep(%d)
			return
		}
	}
	child := exec.Command(os.Args[0], hold)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	_ = child.Start()
}
`

// TestWaitDelayBoundsAGrandchildHoldingTheOutputPipe proves a descendant cannot
// hold a run open forever.
//
// The go command starts compilers and helpers of its own, so the process the
// runner waits for is not always the last one holding the output pipe. Waiting
// for the pipe rather than for the process is the default, and without a delay
// it is unbounded: the stand-in makes that gap explicit by exiting at once and
// leaving a child behind.
func TestWaitDelayBoundsAGrandchildHoldingTheOutputPipe(t *testing.T) {
	runner, err := gocli.New(t.Context(), gocli.Options{
		Binary:  buildStandIn(t),
		Dir:     t.TempDir(),
		Inherit: []string{"PATH"},
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}

	start := time.Now()
	values, callErr := runner.Env(t.Context(), "GOCACHE")
	elapsed := time.Since(start)

	if !errors.Is(callErr, exec.ErrWaitDelay) {
		t.Fatalf("error = %v, want %v", callErr, exec.ErrWaitDelay)
	}
	if values != nil {
		t.Fatalf("abandoned read returned %v", values)
	}
	if elapsed >= standInHold {
		t.Fatalf("the call took %s, so it waited for the grandchild rather than for the delay", elapsed)
	}
}

// buildStandIn compiles the pipe holding stand-in and returns its path.
func buildStandIn(t *testing.T) string {
	t.Helper()
	return buildStandInSource(t, fmt.Sprintf(standInSource, standInHold.Nanoseconds()))
}

// buildStandInSource compiles a one file program and returns its path, so a
// test can put a stand-in for the go command in front of the runner.
func buildStandInSource(t *testing.T, source string) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go is not on PATH: %v", err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module soapbox.test/standin\n\ngo 1.26.0\n")
	writeFile(t, filepath.Join(dir, "main.go"), source)

	binary := filepath.Join(dir, "standin")
	build := exec.CommandContext(t.Context(), goBinary, "build", "-o", binary, ".")
	build.Dir = dir
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the stand-in: %v\n%s", err, out)
	}
	return binary
}
