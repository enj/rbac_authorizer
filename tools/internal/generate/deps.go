package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/modulegraph"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// runDependencies decides which staging packages the generated module owns.
//
// The decision is reached over the complete post-prune module, facade included,
// which is why the facade files are installed into the scratch module before the
// graph is loaded. The facade is the module's public boundary, so a graph that
// did not contain it would be asked which dependencies the boundary needs while
// the boundary was missing, and every answer would be about the relocated
// packages rather than about what a consumer actually compiles.
//
// Installing the facade first also closes the one way the published go.mod could
// be wrong. It was tidied before the facade existed, so a facade import that
// needs a requirement the relocated code does not, or that turns an indirect
// requirement direct, would leave the module metadata describing a tree that is
// not the one being published. Tidying is therefore re-run as a diff: it writes
// nothing and refuses if anything would change.
func (r *run) runDependencies(ctx context.Context) error {
	if err := r.installFacade(ctx); err != nil {
		return err
	}

	graph, err := r.loadModuleGraph(ctx)
	if err != nil {
		return runtimeError(stageDependencies, err)
	}

	// Translating the profile is pure, so a failure is a statement about what
	// the dependency section says.
	options, err := dependencyOptions(r.cfg, r.opts.Ref.Name)
	if err != nil {
		return policyError(stageDependencies, err)
	}
	if err := checkCopySupported(options); err != nil {
		return err
	}

	// Candidates are the staging packages a copy would take ownership of, and
	// this profile proposes none. Stating an empty candidate set is the honest
	// answer rather than a skipped phase: the decision is that nothing is
	// copied, and a report that recorded no decision at all would be
	// indistinguishable from one where the phase never ran.
	depGraph, err := graph.Deppolicy(ctx, modulegraph.DeppolicySpec{
		Boundary:   []string{r.cfg.Destination.Module},
		Candidates: options.Proposals,
	})
	if err != nil {
		return classify(stageDependencies, err, dependencySemantic...)
	}

	decider, err := deppolicy.New(ctx, options)
	if err != nil {
		return classify(stageDependencies, err, dependencySemantic...)
	}
	result, err := decider.Decide(ctx, depGraph)
	if err != nil {
		return classify(stageDependencies, err, dependencySemantic...)
	}
	if len(result.Copy) > 0 {
		return policyError(stageDependencies, fmt.Errorf("%w: the decision approves %d staging copies, and materializing a copied package is not implemented", ErrUnsupported, len(result.Copy)))
	}

	r.report.recordDependencies(result)
	return nil
}

// installFacade writes the generated facade into the post-prune scratch module
// and proves the module metadata still describes it.
func (r *run) installFacade(ctx context.Context) error {
	for _, file := range r.postFacade.Files {
		path := filepath.Join(r.paths.PostModule, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
		}
		if err := os.WriteFile(path, file.Contents, file.Mode.FileMode().Perm()); err != nil {
			return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
		}
	}

	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
	}
	// A tidy that could not run is a toolchain condition. A tidy that ran and
	// reported a difference is the finding: the published metadata would not
	// describe the published tree.
	if err := runner.Tidy(ctx, gocli.TidyOptions{Diff: true}); err != nil {
		if errors.Is(err, gocli.ErrTidyRequired) {
			return policyError(stageDependencies, fmt.Errorf("the generated facade needs module requirements the tidied go.mod does not state, so the published metadata would not describe the published tree: %w", err))
		}
		return runtimeError(stageDependencies, fmt.Errorf("facade install: %w", err))
	}
	return nil
}

// loadModuleGraph type checks the complete generated module.
func (r *run) loadModuleGraph(ctx context.Context) (*modulegraph.Graph, error) {
	runner, err := r.opts.Go.WithDir(r.paths.PostModule)
	if err != nil {
		return nil, fmt.Errorf("module graph: %w", err)
	}
	env, err := runner.LoaderEnv(ctx)
	if err != nil {
		return nil, fmt.Errorf("module graph: %w", err)
	}
	graph, err := modulegraph.Load(ctx, modulegraph.Options{
		Dir:      r.paths.PostModule,
		Env:      env,
		Patterns: []string{"./..."},
		Redactor: runner.Redactor(),
	})
	if err != nil {
		return nil, fmt.Errorf("module graph: %w", err)
	}
	return graph, nil
}

// checkCopySupported refuses a profile that would need a staging copy.
//
// The decision itself is implemented and tested, but nothing downstream can
// carry out a copy: materializing one means reading the staging package's files
// out of the upstream tree at the source commit, relocating them beside the
// extracted code, collecting the licence that governs them, and recording all of
// it in the root provenance. Until that exists, approving a copy would produce a
// module whose provenance describes files the tree does not contain.
func checkCopySupported(options deppolicy.Options) error {
	if len(options.Proposals) == 0 {
		return nil
	}
	return policyError(stageDependencies, fmt.Errorf("%w: the profile proposes %d staging package copies, and materializing a copied package is not implemented", ErrUnsupported, len(options.Proposals)))
}

// dependencyOptions translates the profile's dependency section into the
// decider's shape.
//
// The identity requirements come from the facade's interface assertions rather
// than from a list of their own. An assertion is a promise that a facade type
// implements a real upstream interface, so the module owning that interface can
// never be copied: a copied interface is a different type, and the assertion
// would then prove something about the copy while consumers pass the original.
func dependencyOptions(cfg *config.Config, sourceTag string) (deppolicy.Options, error) {
	minor, err := sourceMinor(sourceTag)
	if err != nil {
		return deppolicy.Options{}, err
	}
	options := deppolicy.Options{
		ModulePath:     cfg.Destination.Module,
		InternalPrefix: cfg.Destination.InternalPrefix,
		SourceMinor:    minor,
		Policy:         cfg.Dependencies.Policy,
		Proposals:      cfg.Dependencies.CopyPackages,
		Gates: deppolicy.Gates{
			Interoperability: cfg.Dependencies.Gates.Interoperability,
			GlobalState:      cfg.Dependencies.Gates.GlobalState,
			Diamond:          cfg.Dependencies.Gates.Diamond,
			Cost: deppolicy.CostCeilings{
				MaxCopiedPackages:   cfg.Dependencies.Gates.Cost.MaxCopiedPackages,
				MaxCopiedLines:      cfg.Dependencies.Gates.Cost.MaxCopiedLines,
				MaxGeneratedFiles:   cfg.Dependencies.Gates.Cost.MaxGeneratedFiles,
				MaxDistinctLicenses: cfg.Dependencies.Gates.Cost.MaxDistinctLicenses,
				MaxModuleZipBytes:   cfg.Dependencies.Gates.Cost.MaxModuleZipBytes,
				MaxReleasesPerMinor: cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor,
				MinModulesRemoved:   cfg.Dependencies.Gates.Cost.MinModulesRemoved,
				MinPackagesRemoved:  cfg.Dependencies.Gates.Cost.MinPackagesRemoved,
				MinLinesRemoved:     cfg.Dependencies.Gates.Cost.MinLinesRemoved,
			},
		},
	}
	for _, assertion := range cfg.Facade.InterfaceAssertions {
		options.IdentityRequired = append(options.IdentityRequired, assertion.Interface)
	}
	for _, override := range cfg.Dependencies.Overrides {
		expires, err := overrideMinor(override.ExpiresAfter)
		if err != nil {
			return deppolicy.Options{}, fmt.Errorf("dependency override for %s: %w", override.Package, err)
		}
		options.Overrides = append(options.Overrides, deppolicy.Override{
			StagingPath:       override.Package,
			Gate:              override.Gate,
			Justification:     override.Justification,
			Approver:          override.Approver,
			ExpiresAfterMinor: expires,
		})
	}
	return options, nil
}

// sourceMinor reads the Kubernetes minor series out of the upstream release tag.
//
// Overrides expire relative to it, so a relaxation granted for one release
// cannot outlive the reason it was granted.
func sourceMinor(sourceTag string) (int, error) {
	version, err := config.ParseSemver(sourceTag)
	if err != nil {
		return 0, fmt.Errorf("source release %q: %w", sourceTag, err)
	}
	return version.Minor, nil
}

// overrideMinor reads the minor series an override is believed through.
func overrideMinor(expiresAfter string) (int, error) {
	version, err := config.ParseSemver(expiresAfter)
	if err != nil {
		return 0, fmt.Errorf("expiry %q: %w", expiresAfter, err)
	}
	return version.Minor, nil
}

// facadeFiles renders the generated facade as relocated files for composition.
func (r *run) facadeFiles() []relocate.File {
	return r.postFacade.Files
}
