package setup

import (
	"fmt"
	"regexp"
	"strings"
)

// pinnedAction is one third party action and the exact commit it is pinned to.
//
// A tag is not a pin. GitHub tags move, and an action that moved is arbitrary
// code running with whatever permissions the job holds, so the SHA is the
// contract and the tag beside it is a comment for the reader. Both are recorded
// here rather than resolved at generation time, because resolving a tag over the
// network would make the generated workflow depend on when setup ran.
type pinnedAction struct {
	// Name is the owner/repository of the action.
	Name string
	// SHA is the full forty character commit the workflow pins.
	SHA string
	// Tag is the release that commit was, recorded only as a comment.
	Tag string
}

// Ref renders the pinned reference a workflow step uses.
func (a pinnedAction) Ref() string {
	return fmt.Sprintf("%s@%s # %s", a.Name, a.SHA, a.Tag)
}

// The actions the generated workflows run. Both SHAs were read from the
// repositories they belong to at the tags named beside them.
var (
	actionCheckout = pinnedAction{Name: "actions/checkout", SHA: "3d3c42e5aac5ba805825da76410c181273ba90b1", Tag: "v7.0.1"}
	actionSetupGo  = pinnedAction{Name: "actions/setup-go", SHA: "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", Tag: "v7.0.0"}
)

// syncSchedule is the cron expression the publishing workflow runs on.
//
// The minute is deliberately not zero. Every repository that asks for a nightly
// run lands on the hour, so the platform sheds load exactly there, and a
// scheduled workflow that is dropped is a sync that silently did not happen.
const syncSchedule = "37 4 * * *"

// syncConcurrency is the group the publishing workflow serialises on. It is a
// constant rather than an expression because there is one publishing pipeline
// per repository, and a group that varied by ref would let two runs publish at
// once.
const syncConcurrency = "soapbox-sync"

// SyncCommand is the single Go invocation the publishing workflow runs.
//
// Every decision about what is published is the engine's. The workflow
// contributes a checkout, a toolchain, three secrets, and the two directories
// the engine may not choose for itself: the repository is both the profile
// directory and the destination the objects are written into, and the source
// cache goes to the runner's scratch space because the engine refuses a cache
// inside the profile directory.
//
// The invocation carries no -apply. A scheduled workflow that published without
// an approval would be an outward action nobody authorized, so enabling
// publication is a deliberate edit made at the outward-action gate rather than a
// default this package ships.
//
// The flags named here are asserted against the ones the sync command actually
// defines, in the command layer that owns both, so the two cannot drift apart
// silently.
const SyncCommand = "go run ./cmd/soapbox sync -dir .. -destination .. -cache ${{ runner.temp }}/soapbox-cache"

// VerifyCommand is the read-only engine invocation the CI workflow runs.
const VerifyCommand = "go run ./cmd/soapbox validate -dir .."

// shaPattern matches a full commit object name, which is the only pin a
// workflow may name.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// secretNamePattern matches a GitHub Actions secret name.
var secretNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// branchPattern matches the branch names the generated workflows may name. It is
// narrower than what Git accepts on purpose: this value is interpolated into a
// YAML scalar and into a workflow expression, and a branch name that needed
// quoting to survive either one would be a branch name that could change what the
// workflow means.
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// goVersionPattern matches the toolchain version the setup-go action installs.
var goVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// workflowInputs are the profile derived values the workflows interpolate.
type workflowInputs struct {
	branch    string
	goVersion string
	secrets   []string
}

// checkWorkflowInputs refuses any value that could change what the generated
// YAML means.
//
// Everything interpolated into a workflow comes from the profile, which is
// checked in, reviewed, and validated. That still is not a reason to interpolate
// it unchecked: a workflow is the one generated artifact that runs with
// credentials, so a value reaching it is checked against what it is allowed to
// be rather than against what would be obviously wrong.
func (w workflowInputs) check() error {
	if !branchPattern.MatchString(w.branch) {
		return fmt.Errorf("workflow: branch %q is not a plain branch name", w.branch)
	}
	if !goVersionPattern.MatchString(w.goVersion) {
		return fmt.Errorf("workflow: Go version %q is not a release version", w.goVersion)
	}
	seen := make(map[string]bool, len(w.secrets))
	for _, secret := range w.secrets {
		switch {
		case !secretNamePattern.MatchString(secret):
			return fmt.Errorf("workflow: secret name %q is not a legal GitHub Actions secret name", secret)
		case strings.HasPrefix(strings.ToUpper(secret), "GITHUB_"):
			return fmt.Errorf("workflow: secret name %q is reserved by GitHub Actions", secret)
		case seen[secret]:
			return fmt.Errorf("workflow: secret name %q is used twice", secret)
		}
		seen[secret] = true
	}
	for _, action := range []pinnedAction{actionCheckout, actionSetupGo} {
		if !shaPattern.MatchString(action.SHA) {
			return fmt.Errorf("workflow: %s is not pinned to a full commit", action.Name)
		}
	}
	return nil
}

// goVersionOf turns a pinned toolchain such as go1.26.5 into the version the
// setup-go action installs.
func goVersionOf(toolchain string) string {
	return strings.TrimPrefix(toolchain, "go")
}

// composeCIWorkflow renders the verification workflow.
//
// It never receives App credentials and it never writes. Pull request code runs
// here, so a token this job held would be a token any contributor could reach,
// and the checkout is told not to persist one at all so a later step cannot use
// it by accident.
func composeCIWorkflow(in workflowInputs) []byte {
	var b strings.Builder
	b.WriteString(`# Generated by soapbox setup. Read-only verification.
#
# This workflow runs on pull request code and therefore holds no credential: no
# App secret is available to it, its token may only read, and the checkout keeps
# no token in the work tree for a later step to find.
name: ci

on:
  push:
    branches:
      - '`)
	b.WriteString(in.branch)
	b.WriteString(`'
  pull_request:
    branches:
      - '`)
	b.WriteString(in.branch)
	b.WriteString(`'

permissions: {}

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  verify:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    permissions:
      contents: read
    steps:
      - name: Check out the repository
        uses: `)
	b.WriteString(actionCheckout.Ref())
	b.WriteString(`
        with:
          persist-credentials: false
      - name: Set up Go
        uses: `)
	b.WriteString(actionSetupGo.Ref())
	b.WriteString(`
        with:
          go-version: '`)
	b.WriteString(in.goVersion)
	b.WriteString(`'
          check-latest: false
      - name: Build
        run: go build ./...
      - name: Vet
        run: go vet ./...
      - name: Test
        run: go test ./...
      - name: Build the engine shim
        working-directory: tools
        run: go build ./...
      - name: Vet the engine shim
        working-directory: tools
        run: go vet ./...
      - name: Test the engine shim
        working-directory: tools
        run: go test ./...
      - name: Validate the profile
        working-directory: `)
	b.WriteString(toolsDirName)
	b.WriteString(`
        run: `)
	b.WriteString(VerifyCommand)
	b.WriteString("\n")
	return []byte(b.String())
}

// composeSyncWorkflow renders the publishing workflow.
//
// Five properties are load bearing and each is visible in the rendered YAML.
// Publishing runs only from the protected default branch, so a fork or a topic
// branch cannot reach the credentials. There is no pull_request_target trigger,
// which is the one trigger that would hand a fork's code the repository's own
// secrets. One non-cancelling concurrency group means a long backfill is never
// killed midway by the next scheduled run. The job contains exactly one Go
// invocation, so every decision about what gets published is the engine's and is
// reviewable in Go rather than spread across workflow steps.
//
// The fifth is the one worth stating twice: the automatic GITHUB_TOKEN is read
// only. Writes are made with the installation token the engine mints from the
// App secrets, which is scoped to the repositories the App is installed on and
// expires in an hour. Granting the workflow token contents: write as well would
// add a second, longer lived way to write that nothing in the run would use.
func composeSyncWorkflow(in workflowInputs) []byte {
	var b strings.Builder
	b.WriteString(`# Generated by soapbox setup. Publishing.
#
# This is the only workflow that holds App credentials. It runs on a schedule or
# an explicitly authorized manual dispatch, never on pull request code, and the
# job refuses to run from any ref but the protected default branch. All of the
# maintained logic is one Go invocation; the workflow supplies a checkout, a
# toolchain, and the three secrets the engine mints its installation token from.
#
# The invocation below plans and does not publish. Publication is enabled at the
# outward-action gate by adding -apply and the approval it requires.
name: sync

on:
  schedule:
    - cron: '`)
	b.WriteString(syncSchedule)
	b.WriteString(`'
  workflow_dispatch:

permissions: {}

concurrency:
  group: `)
	b.WriteString(syncConcurrency)
	b.WriteString(`
  cancel-in-progress: false

jobs:
  sync:
    timeout-minutes: 180
    if: github.ref == 'refs/heads/`)
	b.WriteString(in.branch)
	b.WriteString(`'
    runs-on: ubuntu-latest
    permissions:
      # Reading this repository is all the workflow token is for. Branches and
      # tags are advanced with the short lived App installation token the engine
      # mints from the secrets below, so a workflow token that could write would
      # be a second way in that no step uses.
      contents: read
      # Each successful run checks that this workflow is still enabled, which is
      # how the sixty day schedule disablement is noticed before it matters.
      actions: read
    steps:
      - name: Check out the repository
        uses: `)
	b.WriteString(actionCheckout.Ref())
	b.WriteString(`
        with:
          persist-credentials: false
      - name: Set up Go
        uses: `)
	b.WriteString(actionSetupGo.Ref())
	b.WriteString(`
        with:
          go-version: '`)
	b.WriteString(in.goVersion)
	b.WriteString(`'
          check-latest: false
      - name: Synchronize with upstream
        working-directory: `)
	b.WriteString(toolsDirName)
	b.WriteString(`
        env:
`)
	for _, secret := range in.secrets {
		fmt.Fprintf(&b, "          %s: ${{ secrets.%s }}\n", secret, secret)
	}
	b.WriteString(`        run: `)
	b.WriteString(SyncCommand)
	b.WriteString("\n")
	return []byte(b.String())
}
