package typeswap

import (
	"slices"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// Generator directive keys this analysis understands.
//
// The two conversion keys are the ones that establish a pairing. The rest are
// collected because they describe what upstream generated for the internal
// package, which is what makes a marker dangling once its output is pruned.
const (
	// MarkerConversionGen names the internal package a versioned package
	// converts to and from.
	MarkerConversionGen = "k8s:conversion-gen"
	// MarkerConversionExternal names the published package the conversions
	// target.
	MarkerConversionExternal = "k8s:conversion-gen-external-types"
	// MarkerGroupName is the API group both sides belong to.
	MarkerGroupName = "groupName"
	// MarkerDeepCopyGen, MarkerDefaulterGen, MarkerProtobufGen, and
	// MarkerOpenAPIGen describe generated output that pruning may remove.
	MarkerDeepCopyGen  = "k8s:deepcopy-gen"
	MarkerDefaulterGen = "k8s:defaulter-gen"
	MarkerProtobufGen  = "k8s:protobuf-gen"
	MarkerOpenAPIGen   = "k8s:openapi-gen"
)

// Marker is one parsed upstream generator directive.
type Marker struct {
	// Package is the package that declares it.
	Package string
	// File is the base name of the file it appears in.
	File string
	// Key is the directive key, without the leading plus.
	Key string
	// Value is the directive value, or the empty string for a bare directive.
	Value string
	// Position locates it.
	Position string
}

// analyzeMarkers proves that upstream itself records the pairing.
//
// This proof exists because the alternative is guessing. Two packages whose
// types have the same names and the same fields are not necessarily two
// spellings of one API, and an engine that decided they were would eventually
// substitute two types that merely resemble each other. Upstream already made
// this decision when it wired conversion-gen, so the analysis reads that
// decision instead of forming its own.
func analyzeMarkers(graph *Graph, pair Pair) AnalysisReport {
	markers := collectMarkers(graph)

	var evidence, blockers []string
	// The two directives have to come from the same package, and the pairing is
	// recorded per file because that is how upstream writes it: one doc.go
	// names the internal package it converts from and the published package it
	// converts to. Tracking two independent booleans instead would accept a
	// tree where some unrelated package names the internal side and a different
	// unrelated package names the external side, which pairs nothing.
	paired := map[string][]Marker{}
	for _, marker := range markers {
		if marker.Key != MarkerConversionGen && marker.Key != MarkerConversionExternal {
			continue
		}
		key := marker.Package + "\x00" + marker.File
		paired[key] = append(paired[key], marker)
	}

	var pairedIn []string
	for _, key := range slices.Sorted(mapKeysOfMarkers(paired)) {
		var internalMarker, externalMarker *Marker
		for i := range paired[key] {
			marker := &paired[key][i]
			if marker.Key == MarkerConversionGen && marker.Value == pair.Internal {
				internalMarker = marker
			}
			if marker.Key == MarkerConversionExternal && marker.Value == pair.External {
				externalMarker = marker
			}
		}
		if internalMarker == nil || externalMarker == nil {
			continue
		}
		pairedIn = append(pairedIn, internalMarker.Package)
		evidence = append(evidence,
			renderMarker(*internalMarker, "and +"+externalMarker.Key+"="+externalMarker.Value+
				" in the same file pair the internal package with the published one"))
	}

	// A shared group name is corroboration rather than proof: it says the two
	// packages describe the same API group, which is necessary for a pairing
	// and nowhere near sufficient for one.
	internalGroup := groupName(markers, pair.Internal)
	externalGroup := groupName(markers, pair.External)
	switch {
	case internalGroup != "" && internalGroup == externalGroup:
		evidence = append(evidence, "both packages declare API group "+internalGroup)
	case internalGroup != "" && externalGroup != "" && internalGroup != externalGroup:
		blockers = append(blockers, "the packages declare different API groups, "+
			internalGroup+" and "+externalGroup+", so they do not describe the same API")
	}

	if len(pairedIn) == 0 {
		blockers = append(blockers, "no upstream generator directive pairs "+pair.Internal+" with "+pair.External+
			" in one package; the engine will not infer a pairing from matching type names, nor from the two halves appearing in unrelated packages")
		// A loader that parsed without comments produces exactly the same
		// symptom as a tree that records no pairing, and the two call for
		// completely different fixes. Saying which one this is costs one line
		// and saves an operator from looking for a missing directive that is
		// sitting in the source.
		if len(markers) == 0 {
			blockers = append(blockers,
				"no generator directive was found anywhere in the graph, which usually means the packages were parsed without comments rather than that upstream records no pairing")
		}
	}

	return analysisReport(AnalysisMarkers, evidence, blockers)
}

// collectMarkers parses every generator directive in the graph, sorted.
func collectMarkers(graph *Graph) []Marker {
	var markers []Marker
	for _, pkg := range graph.Packages {
		for i, file := range pkg.Syntax {
			name := ""
			if i < len(pkg.CompiledGoFiles) {
				name = pkg.CompiledGoFiles[i]
			}
			for _, group := range file.Comments {
				for _, comment := range group.List {
					// The rewrite package owns what a directive is, because it
					// is the package that will later rewrite or remove one.
					directive, ok := rewrite.ParseDirective(comment.Text)
					if !ok || directive.Kind != rewrite.MarkerDirective {
						continue
					}
					markers = append(markers, Marker{
						Package:  pkg.ImportPath,
						File:     name,
						Key:      directive.Key,
						Value:    directive.Value,
						Position: graph.position(comment.Pos()),
					})
				}
			}
		}
	}
	slices.SortFunc(markers, compareMarkers)
	return markers
}

// groupName returns the API group one package declares, or the empty string.
func groupName(markers []Marker, pkgPath string) string {
	for _, marker := range markers {
		if marker.Key == MarkerGroupName && marker.Package == pkgPath {
			return marker.Value
		}
	}
	return ""
}

// danglingMarkers returns the directives in retained files whose target package
// is being pruned.
//
// These are the markers the rewrite phase strips. They are identified here
// because this is where the pruning decision is known: a directive naming a
// package that survives is fine, and the same directive naming a package that
// does not is a reference to something the generated module will not contain.
// A generator run against the published module would then fail or, worse,
// silently regenerate nothing.
func danglingMarkers(graph *Graph, pruned string) []Marker {
	retained := make(map[string]bool, len(graph.Retained))
	for _, importPath := range graph.Retained {
		retained[importPath] = true
	}
	rules := rewrite.DefaultRules()

	prunedOutputs := graph.PrunedGeneratedOutputs()

	var dangling []Marker
	for _, marker := range collectMarkers(graph) {
		if !retained[marker.Package] {
			continue
		}
		// A marker dangles when the package it names is pruned, and equally
		// when the file it used to generate is. The second case has no value to
		// compare against, so it is matched on the generator kind instead.
		namesPruned := marker.Value == pruned
		outputPruned := slices.Contains(prunedOutputs, marker.Key)
		if !namesPruned && !outputPruned {
			continue
		}
		// Only a directive the rewrite phase actually removes is a behaviour
		// change. DefaultRules keeps some markers that name a pruned package on
		// purpose, notably the external types marker, which points at a
		// published package that is never relocated. Reporting a kept marker as
		// stripped would put a change in provenance that never happens.
		if !rules.RemovesWhenDangling(marker.Key) {
			continue
		}
		dangling = append(dangling, marker)
	}
	slices.SortFunc(dangling, compareMarkers)
	return dangling
}

// mapKeysOfMarkers yields a marker map's keys so callers can sort them.
func mapKeysOfMarkers(m map[string][]Marker) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// renderMarker writes one marker as an evidence line.
func renderMarker(marker Marker, detail string) string {
	line := "+" + marker.Key
	if marker.Value != "" {
		line += "=" + marker.Value
	}
	line += " in " + marker.Package
	if marker.File != "" {
		line += "/" + marker.File
	}
	return line + " " + detail
}

// compareMarkers orders markers so a report is byte stable.
func compareMarkers(a, b Marker) int {
	if c := compareStrings(a.Package, b.Package); c != 0 {
		return c
	}
	if c := compareStrings(a.File, b.File); c != 0 {
		return c
	}
	if c := compareStrings(a.Key, b.Key); c != 0 {
		return c
	}
	return compareStrings(a.Value, b.Value)
}
