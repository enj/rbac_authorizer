package setup_test

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/enj/soapbox/tools/internal/setup"
)

// pinnedUses matches an action reference pinned to a full commit. The release
// that commit was is a YAML comment beside it, which the decoder strips, so the
// comment is asserted separately against the file text.
var pinnedUses = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+@[0-9a-f]{40}$`)

// pinnedUsesLine matches the whole rendered line, the pin and the release a
// reader needs to know which one it is.
var pinnedUsesLine = regexp.MustCompile(`^ *(- )?uses: [A-Za-z0-9._-]+/[A-Za-z0-9._-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$`)

// workflow is the shape of a generated workflow, decoded rather than matched.
//
// Decoding is what makes these assertions mean anything. A test that grepped for
// "contents: read" would pass on a workflow where that string appeared in a
// comment, in the wrong job, or beside a second permissions block that overrode
// it, and every one of those is the bug this file exists to catch.
type workflow struct {
	Name string `yaml:"name"`
	// The trigger key is quoted because YAML 1.1 resolves a bare "on" to a
	// boolean, and the generated file is read back by GitHub rather than by this
	// decoder.
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency concurrency            `yaml:"concurrency"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type concurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type workflowJob struct {
	If             string            `yaml:"if"`
	RunsOn         string            `yaml:"runs-on"`
	TimeoutMinutes int               `yaml:"timeout-minutes"`
	Permissions    map[string]string `yaml:"permissions"`
	Steps          []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	With             map[string]any    `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
}

// TestGeneratedWorkflowsAreLeastPrivilege inspects what setup actually wrote.
func TestGeneratedWorkflowsAreLeastPrivilege(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)
	if _, err := setup.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("apply: %v", err)
	}

	ci := decodeWorkflow(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	sync := decodeWorkflow(t, filepath.Join(root, ".github", "workflows", "sync.yml"))

	t.Run("every job has a bounded runtime", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			for jobName, job := range flow.Jobs {
				if job.TimeoutMinutes <= 0 {
					t.Errorf("%s job %s has no timeout", name, jobName)
				}
			}
		}
	})

	t.Run("no workflow inherits a token by default", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			if len(flow.Permissions) != 0 {
				t.Errorf("%s grants %v at the top level, want none", name, flow.Permissions)
			}
		}
	})

	t.Run("no workflow runs fork code with repository secrets", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			if _, ok := flow.On["pull_request_target"]; ok {
				t.Errorf("%s uses pull_request_target", name)
			}
		}
	})

	t.Run("every action is pinned to a commit", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			for _, job := range flow.Jobs {
				for _, step := range job.Steps {
					if step.Uses == "" {
						continue
					}
					if !pinnedUses.MatchString(step.Uses) {
						t.Errorf("%s step %q uses %q, which is not a full commit pin", name, step.Name, step.Uses)
					}
				}
			}
		}
		// A bare SHA is unreadable, so every pin carries the release it was. The
		// comment is not the contract, but a pin nobody can identify is a pin
		// nobody will ever update.
		for _, name := range []string{"ci.yml", "sync.yml"} {
			for _, line := range strings.Split(readFile(t, filepath.Join(root, ".github", "workflows", name)), "\n") {
				if !strings.Contains(line, "uses:") {
					continue
				}
				if !pinnedUsesLine.MatchString(line) {
					t.Errorf("%s line %q does not name the release its pin was", name, line)
				}
			}
		}
	})

	t.Run("ci holds no credential", func(t *testing.T) {
		job, ok := ci.Jobs["verify"]
		if !ok {
			t.Fatalf("ci has jobs %v, want verify", keysOf(ci.Jobs))
		}
		if got := job.Permissions; len(got) != 1 || got["contents"] != "read" {
			t.Errorf("ci verify permissions = %v, want only contents: read", got)
		}
		for _, step := range job.Steps {
			if len(step.Env) != 0 {
				t.Errorf("ci step %q exports %v, and a verification job needs no secret", step.Name, step.Env)
			}
		}
		if strings.Contains(readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml")), "secrets.") {
			t.Error("ci names a secret")
		}
		if got := checkoutWith(t, job, "persist-credentials"); got != false {
			t.Errorf("ci checkout persist-credentials = %v, want false", got)
		}
	})

	t.Run("sync runs unattended and only from the default branch", func(t *testing.T) {
		if _, ok := sync.On["workflow_dispatch"]; !ok {
			t.Error("sync cannot be dispatched manually")
		}
		if _, ok := sync.On["pull_request"]; ok {
			t.Error("sync runs on pull requests")
		}
		if want := "github.ref == 'refs/heads/main'"; sync.Jobs["sync"].If != want {
			t.Errorf("sync guard = %q, want %q", sync.Jobs["sync"].If, want)
		}
	})

	t.Run("sync is scheduled off the hour", func(t *testing.T) {
		schedule, ok := sync.On["schedule"].([]any)
		if !ok || len(schedule) != 1 {
			t.Fatalf("sync schedule = %v, want exactly one entry", sync.On["schedule"])
		}
		entry, ok := schedule[0].(map[string]any)
		if !ok {
			t.Fatalf("sync schedule entry = %v, want a cron mapping", schedule[0])
		}
		cron, ok := entry["cron"].(string)
		if !ok {
			t.Fatalf("sync schedule entry = %v, want a cron expression", entry)
		}
		minute, _, ok := strings.Cut(cron, " ")
		if !ok {
			t.Fatalf("cron %q has no minute field", cron)
		}
		value, err := strconv.Atoi(minute)
		if err != nil {
			t.Fatalf("cron minute %q: %v", minute, err)
		}
		// The busy minutes are the ones every other repository asks for. A run
		// scheduled there is the one most likely to be dropped under load.
		if value == 0 || value == 30 {
			t.Errorf("cron minute = %d, want a minute other repositories do not crowd", value)
		}
	})

	t.Run("sync serialises without cancelling", func(t *testing.T) {
		if sync.Concurrency.CancelInProgress {
			t.Error("sync cancels a run in progress, which would kill a backfill midway")
		}
		if sync.Concurrency.Group == "" || strings.Contains(sync.Concurrency.Group, "${{") {
			t.Errorf("sync concurrency group = %q, want one fixed group per repository", sync.Concurrency.Group)
		}
	})

	t.Run("sync writes with the App token and not the workflow token", func(t *testing.T) {
		job := sync.Jobs["sync"]
		for permission, level := range job.Permissions {
			if level == "write" {
				t.Errorf("sync grants the workflow token %s: write, which no step uses", permission)
			}
		}
		if job.Permissions["contents"] != "read" || job.Permissions["actions"] != "read" {
			t.Errorf("sync permissions = %v, want contents and actions read", job.Permissions)
		}
	})

	t.Run("sync exports exactly the profile's App secrets", func(t *testing.T) {
		var exported map[string]string
		for _, step := range sync.Jobs["sync"].Steps {
			if len(step.Env) > 0 {
				if exported != nil {
					t.Fatal("more than one sync step exports secrets")
				}
				exported = step.Env
			}
		}
		want := map[string]string{
			"SOAPBOX_GITHUB_APP_ID":          "${{ secrets.SOAPBOX_GITHUB_APP_ID }}",
			"SOAPBOX_GITHUB_INSTALLATION_ID": "${{ secrets.SOAPBOX_GITHUB_INSTALLATION_ID }}",
			"SOAPBOX_GITHUB_APP_PRIVATE_KEY": "${{ secrets.SOAPBOX_GITHUB_APP_PRIVATE_KEY }}",
		}
		if len(exported) != len(want) {
			t.Fatalf("sync exports %v, want %v", exported, want)
		}
		for name, value := range want {
			if exported[name] != value {
				t.Errorf("sync exports %s = %q, want %q", name, exported[name], value)
			}
		}
	})

	t.Run("sync runs one Go command and publishes nothing by default", func(t *testing.T) {
		var commands []string
		for _, step := range sync.Jobs["sync"].Steps {
			if step.Run != "" {
				commands = append(commands, step.Run)
			}
		}
		if len(commands) != 1 {
			t.Fatalf("sync runs %d commands, want exactly one: %q", len(commands), commands)
		}
		if !strings.HasPrefix(commands[0], "go run ") {
			t.Errorf("sync command = %q, want one Go invocation", commands[0])
		}
		if strings.Contains(commands[0], "-apply") {
			t.Error("the generated sync workflow publishes without an approval")
		}
		if strings.ContainsAny(commands[0], "|&;<>()") {
			t.Errorf("sync command %q composes shell rather than running one program", commands[0])
		}
	})
}

// checkoutWith reads one input of the checkout step.
func checkoutWith(tb testing.TB, job workflowJob, key string) any {
	tb.Helper()
	for _, step := range job.Steps {
		if !strings.HasPrefix(step.Uses, "actions/checkout@") {
			continue
		}
		value, ok := step.With[key]
		if !ok {
			tb.Fatalf("checkout step has no %s input", key)
		}
		return value
	}
	tb.Fatal("no checkout step")
	return nil
}

// decodeWorkflow reads one generated workflow as YAML.
func decodeWorkflow(tb testing.TB, path string) workflow {
	tb.Helper()
	var flow workflow
	if err := yaml.Unmarshal([]byte(readFile(tb, path)), &flow); err != nil {
		tb.Fatalf("decode %s: %v", path, err)
	}
	if flow.Name == "" {
		tb.Fatalf("%s decoded with no name, so the document did not parse as a workflow", path)
	}
	if len(flow.On) == 0 {
		tb.Fatalf("%s decoded with no triggers", path)
	}
	if len(flow.Jobs) == 0 {
		tb.Fatalf("%s decoded with no jobs", path)
	}
	return flow
}

// keysOf lists a map's keys for a failure message.
func keysOf[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}
