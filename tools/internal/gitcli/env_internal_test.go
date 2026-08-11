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

func TestBuildEnvDropsAmbientVariables(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "Ambient Identity")
	t.Setenv("SOAPBOX_UNRELATED", "value")

	env := buildEnv(nil, []string{"SOAPBOX_TOKEN=secret"})
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
