package gitcli

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestFixedEnvironmentIsolatesAmbientConfiguration pins the isolation contract
// that every runner, not only the ones tests build, must apply.
func TestFixedEnvironmentIsolatesAmbientConfiguration(t *testing.T) {
	want := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	}
	for _, entry := range want {
		if !slices.Contains(fixedEnv, entry) {
			t.Errorf("fixed environment is missing %q", entry)
		}
	}
	if got := strings.Join(fixedConfig, " "); got != "-c core.hooksPath="+os.DevNull {
		t.Errorf("fixed configuration = %q, want hooks disabled", got)
	}
}

func TestAssembleEnvDropsAmbientVariables(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Ambient Identity")
	t.Setenv("SOAPBOX_UNRELATED", "value")

	env := assembleEnv(inheritedEnv(nil), nil, []string{"SOAPBOX_TOKEN=secret"})
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GIT_AUTHOR_NAME", "SOAPBOX_UNRELATED":
			t.Errorf("subprocess environment carries ambient %q", entry)
		}
	}
	if !slices.Contains(env, "SOAPBOX_TOKEN=secret") {
		t.Error("caller supplied entries are missing from the subprocess environment")
	}
	// Caller entries come last so they win over the inherited and fixed ones.
	if env[len(env)-1] != "SOAPBOX_TOKEN=secret" {
		t.Errorf("caller entry is not last: %v", env)
	}
}

// TestAssembleEnvOrdersIsolationBeforeCredentials pins the precedence the two
// caller supplied channels have: isolation entries shape where git looks and
// may be overridden by an explicit Env entry, and both outrank the fixed set.
func TestAssembleEnvOrdersIsolationBeforeCredentials(t *testing.T) {
	env := assembleEnv([]string{"HOME=/process"}, []string{"HOME=/isolated"}, []string{"HOME=/explicit"})
	last := ""
	for _, entry := range env {
		if name, value, _ := strings.Cut(entry, "="); name == "HOME" {
			last = value
		}
	}
	if last != "/explicit" {
		t.Errorf("effective HOME = %q, want the caller supplied entry to win", last)
	}
	if got := slices.Index(env, "HOME=/isolated"); got < 0 {
		t.Error("isolation entry is missing from the subprocess environment")
	} else if got < slices.Index(env, "GIT_TERMINAL_PROMPT=0") {
		t.Error("isolation entries must be applied after the fixed entries so they can override one")
	}
}

// TestInheritedEnvSnapshotsTheProcess proves the inherited values are read once,
// so a runner cannot change behaviour because the process environment moved
// under it between construction and use.
func TestInheritedEnvSnapshotsTheProcess(t *testing.T) {
	t.Setenv("SOAPBOX_SNAPSHOT", "first")
	snapshot := inheritedEnv([]string{"SOAPBOX_SNAPSHOT"})
	t.Setenv("SOAPBOX_SNAPSHOT", "second")

	if !slices.Equal(snapshot, []string{"SOAPBOX_SNAPSHOT=first"}) {
		t.Fatalf("inherited snapshot = %v, want the value read at construction", snapshot)
	}
}

func TestEnvValues(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    []string
	}{
		{name: "none", entries: nil, want: []string{}},
		{name: "value", entries: []string{"TOKEN=abc"}, want: []string{"abc"}},
		{name: "empty value is skipped", entries: []string{"TOKEN="}, want: []string{}},
		{name: "no separator is skipped", entries: []string{"TOKEN"}, want: []string{}},
		{name: "value with separators", entries: []string{"URL=https://x/y=z"}, want: []string{"https://x/y=z"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := envValues(test.entries)
			if !slices.Equal(got, test.want) {
				t.Fatalf("envValues(%v) = %v, want %v", test.entries, got, test.want)
			}
		})
	}
}
