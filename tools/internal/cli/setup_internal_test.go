package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/setup"
)

// TestGeneratedWorkflowCommandsNameRealFlags ties the workflows setup writes to
// the commands they invoke.
//
// The setup package composes those command lines and this package defines the
// flags they name, so nothing else in the build connects the two. Without this
// test, renaming or removing a flag would leave a generated workflow that parses
// as YAML, passes review, and fails on the first scheduled run in a repository
// nobody is watching.
func TestGeneratedWorkflowCommandsNameRealFlags(t *testing.T) {
	syncFlags, _ := syncFlagSet()
	validateFlags, _ := validateFlagSet()

	tests := []struct {
		name    string
		command string
		verb    string
		flags   func(string) bool
	}{
		{
			name:    "sync",
			command: setup.SyncCommand,
			verb:    "sync",
			flags:   func(flag string) bool { return syncFlags.Lookup(flag) != nil },
		},
		{
			name:    "validate",
			command: setup.VerifyCommand,
			verb:    "validate",
			flags:   func(flag string) bool { return validateFlags.Lookup(flag) != nil },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := strings.Fields(test.command)
			// The invocation runs one Go program with the engine's own command
			// name, which is what "one Go command" in the workflow means.
			if len(fields) < 4 || fields[0] != "go" || fields[1] != "run" || fields[2] != "./cmd/soapbox" {
				t.Fatalf("command %q is not a go run of the engine shim", test.command)
			}
			if fields[3] != test.verb {
				t.Fatalf("command %q runs %q, want %q", test.command, fields[3], test.verb)
			}
			named := 0
			for _, field := range fields[4:] {
				if !strings.HasPrefix(field, "-") {
					continue
				}
				named++
				flag := strings.TrimPrefix(field, "-")
				if name, _, ok := strings.Cut(flag, "="); ok {
					flag = name
				}
				if !test.flags(flag) {
					t.Errorf("command %q names -%s, which soapbox %s does not define", test.command, flag, test.verb)
				}
			}
			if named == 0 {
				t.Errorf("command %q names no flag, so this test proves nothing", test.command)
			}
		})
	}
}

// TestSyncWorkflowCommandPublishesNothing keeps the generated schedule from
// becoming an outward action by accident. Publication is enabled deliberately,
// at the outward-action gate, and not by a template this package ships.
func TestSyncWorkflowCommandPublishesNothing(t *testing.T) {
	fields := strings.Fields(setup.SyncCommand)
	for _, forbidden := range []string{"-apply", "-approve"} {
		if slices.Contains(fields, forbidden) {
			t.Errorf("the generated sync workflow names %s", forbidden)
		}
	}
}
