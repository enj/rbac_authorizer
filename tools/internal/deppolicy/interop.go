package deppolicy

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

// maxInteropDepth bounds the type walk.
//
// Recursive types are already handled by the visited set; the bound exists for
// the shapes that are finite but pathological, such as deeply instantiated
// generics, so that a strange dependency makes the run slow to reason about
// rather than unable to finish.
const maxInteropDepth = 64

// interopFinding is one place a candidate owned type reaches the public
// boundary.
type interopFinding struct {
	// Candidate is the import path of the package that owns the type.
	Candidate string
	// Type is the fully qualified name of the type that would be relocated.
	Type string
	// Shape is what the type is underneath: interface, struct, basic, slice,
	// map, func, chan, pointer, or array. It is reported because the shape is
	// what decides whether relocation could ever be safe, and an operator
	// reading "interface" knows to look at the method set while one reading
	// "basic" knows there is nothing to look at.
	Shape string
	// Path is the chain from an exported boundary symbol to the type, which is
	// what makes the finding actionable rather than merely true.
	Path string
	// Position is where the type is declared.
	Position string
	// Blocking is always true. The field is kept so the report's shape says out
	// loud that reaching the boundary is the refusal, rather than leaving a
	// reader to infer that every recorded finding happens to be fatal.
	Blocking bool
	// Detail is the specific reason this type cannot cross, which is what an
	// operator acts on.
	Detail string
}

// interopWalker walks every type reachable from the public boundary.
type interopWalker struct {
	fset     *token.FileSet
	owned    map[string]bool
	identity map[string]bool
	findings []interopFinding
	// visited stops recursive types from looping. It is keyed by the named
	// type's object because two instantiations of one generic type share an
	// object and walking either one answers the question for both.
	visited map[*types.TypeName]bool
	// reported deduplicates findings for one type reached by several paths.
	// The first path wins, and the walk is deterministic, so which one that is
	// does not vary between runs.
	reported map[string]bool
	// exhausted records paths the depth bound cut short, so a walk that did not
	// finish cannot be mistaken for one that found nothing.
	exhausted []string
}

// evaluateInterop runs the interoperability gate for every candidate.
//
// The gate asks one question: would relocating this package change the identity
// of a type a consumer can see. Go type identity is nominal, so a relocated
// declaration is a different type from the original even when the source is
// byte for byte the same. A consumer holding the real
// k8s.io/apiserver/pkg/authorization/authorizer.Attributes cannot pass it to a
// function that wants the copy, and no amount of care in the copy fixes that.
//
// Interfaces are the exception, because a caller satisfies an interface
// structurally rather than by name. A relocated interface still accepts the
// consumer's implementations, so copying one is safe when two further things
// hold: no candidate owned type appears anywhere in the transitive method set,
// since such a type would reintroduce a nominal identity through the back door,
// and nothing requires the interface's real identity, since a facade that must
// satisfy the upstream interface needs the upstream interface.
func (d *Decider) evaluateInterop(graph *Graph, owned map[string]bool) (map[string][]interopFinding, []string) {
	byCandidate := make(map[string][]interopFinding, len(graph.Candidates))
	if len(owned) == 0 {
		return byCandidate, nil
	}

	walker := &interopWalker{
		fset:     graph.Fset,
		owned:    owned,
		identity: identityIndex(d.opts.IdentityRequired),
		visited:  make(map[*types.TypeName]bool),
		reported: make(map[string]bool),
	}

	boundary := slices.Clone(graph.Boundary)
	slices.SortFunc(boundary, func(a, b *Package) int {
		return compareStrings(a.ImportPath, b.ImportPath)
	})
	for _, pkg := range boundary {
		scope := pkg.Types.Scope()
		// Scope.Names is sorted, so the walk order, and therefore which path is
		// reported for a type reachable several ways, is stable.
		for _, name := range scope.Names() {
			object := scope.Lookup(name)
			if object == nil || !object.Exported() {
				continue
			}
			root := pkg.Types.Path() + "." + name
			walker.walk(object.Type(), []string{root}, 0)
		}
	}

	for _, finding := range walker.findings {
		byCandidate[finding.Candidate] = append(byCandidate[finding.Candidate], finding)
	}
	for candidate := range byCandidate {
		slices.SortFunc(byCandidate[candidate], compareInteropFindings)
	}
	slices.Sort(walker.exhausted)
	return byCandidate, slices.Compact(walker.exhausted)
}

// identityIndex indexes the fully qualified types whose real identity must
// survive.
func identityIndex(required []string) map[string]bool {
	index := make(map[string]bool, len(required))
	for _, name := range required {
		index[name] = true
	}
	return index
}

// walk descends one type, recording every candidate owned type it reaches.
func (w *interopWalker) walk(typ types.Type, path []string, depth int) {
	if typ == nil {
		return
	}
	if depth > maxInteropDepth {
		// Running out of depth is not a clean walk. Whatever lay beyond was
		// never examined, so reporting nothing would be the absence of evidence
		// standing in for evidence of absence on a correctness gate.
		w.exhausted = append(w.exhausted, strings.Join(path, " -> "))
		return
	}

	switch t := typ.(type) {
	case *types.Alias:
		// An alias is a second spelling of one type, not a second type, so the
		// path keeps the alias name while the analysis continues on the real
		// type. Losing the alias here would report a finding an operator
		// cannot locate in the source they wrote.
		w.walk(types.Unalias(t), path, depth+1)
	case *types.Named:
		w.walkNamed(t, path, depth)
	case *types.Pointer:
		w.walk(t.Elem(), append(path, "*"), depth+1)
	case *types.Slice:
		w.walk(t.Elem(), append(path, "[]"), depth+1)
	case *types.Array:
		w.walk(t.Elem(), append(path, "[N]"), depth+1)
	case *types.Chan:
		w.walk(t.Elem(), append(path, "chan"), depth+1)
	case *types.Map:
		w.walk(t.Key(), append(path, "map key"), depth+1)
		w.walk(t.Elem(), append(path, "map value"), depth+1)
	case *types.Struct:
		for i := range t.NumFields() {
			field := t.Field(i)
			// Only exported fields cross the boundary. An unexported field of a
			// boundary struct is not reachable by a consumer, so a candidate
			// type there is not an interoperability problem; it is a copied
			// lines problem, which the cost gates measure.
			if !field.Exported() {
				continue
			}
			w.walk(field.Type(), append(path, "field "+field.Name()), depth+1)
		}
	case *types.Signature:
		w.walkTuple(t.Params(), path, "parameter", depth)
		w.walkTuple(t.Results(), path, "result", depth)
	case *types.Interface:
		w.walkInterface(t, path, depth)
	case *types.TypeParam:
		w.walk(t.Constraint(), append(path, "constraint"), depth+1)
	case *types.Tuple:
		w.walkTuple(t, path, "element", depth)
	}
}

// walkTuple descends a parameter or result list.
func (w *interopWalker) walkTuple(tuple *types.Tuple, path []string, role string, depth int) {
	if tuple == nil {
		return
	}
	for i := range tuple.Len() {
		variable := tuple.At(i)
		label := fmt.Sprintf("%s %d", role, i)
		if variable.Name() != "" {
			label = role + " " + variable.Name()
		}
		w.walk(variable.Type(), append(path, label), depth+1)
	}
}

// walkInterface descends an interface's own and embedded methods.
func (w *interopWalker) walkInterface(iface *types.Interface, path []string, depth int) {
	methods := make([]*types.Func, 0, iface.NumExplicitMethods())
	for i := range iface.NumExplicitMethods() {
		methods = append(methods, iface.ExplicitMethod(i))
	}
	slices.SortFunc(methods, func(a, b *types.Func) int { return compareStrings(a.Name(), b.Name()) })
	for _, method := range methods {
		if !method.Exported() {
			continue
		}
		w.walk(method.Type(), append(path, "method "+method.Name()), depth+1)
	}
	for i := range iface.NumEmbeddeds() {
		w.walk(iface.EmbeddedType(i), append(path, "embedded"), depth+1)
	}
}

// walkNamed handles the one case the gate actually decides.
func (w *interopWalker) walkNamed(named *types.Named, path []string, depth int) {
	object := named.Obj()
	if object == nil || object.Pkg() == nil {
		return
	}
	qualified := object.Pkg().Path() + "." + object.Name()

	if w.owned[object.Pkg().Path()] {
		// The reported set is consulted before the reason is computed, because
		// computing it walks a transitive method set and this type may already
		// have been decided through another path.
		if w.reported[qualified] {
			return
		}
		w.record(interopFinding{
			Candidate: object.Pkg().Path(),
			Type:      qualified,
			Shape:     shapeOf(named.Underlying()),
			Path:      strings.Join(append(path, qualified), " -> "),
			Position:  w.position(object.Pos()),
			Blocking:  true,
			Detail:    w.blockingReason(named, qualified),
		})
		// A decided type is not descended into. Continuing would report the
		// same refusal again through every field of the same declaration, and
		// one exact path to the boundary is what an operator acts on.
		return
	}

	if w.visited[object] {
		return
	}
	w.visited[object] = true

	// The type's own name joins the path. Without it a finding reads as
	// "New -> result 0 -> parameter attrs", which names neither the type that
	// carries the offending method nor anything an operator can search for.
	// A named type reached as the exported symbol of its own package is
	// already spelled by the root, so it is not spelled twice.
	if len(path) == 0 || path[len(path)-1] != qualified {
		path = append(path, qualified)
	}

	for i := range named.TypeArgs().Len() {
		w.walk(named.TypeArgs().At(i), append(path, "type argument"), depth+1)
	}
	w.walk(named.Underlying(), path, depth+1)

	// Methods are walked through the pointer method set so promoted methods
	// count. A boundary struct that embeds a candidate type exposes that
	// type's methods as its own, and those signatures cross the boundary just
	// as directly as a declared method's would.
	if _, isInterface := named.Underlying().(*types.Interface); !isInterface {
		methodSet := types.NewMethodSet(types.NewPointer(named))
		selections := make([]*types.Selection, 0, methodSet.Len())
		for i := range methodSet.Len() {
			selections = append(selections, methodSet.At(i))
		}
		slices.SortFunc(selections, func(a, b *types.Selection) int {
			return compareStrings(a.Obj().Name(), b.Obj().Name())
		})
		for _, selection := range selections {
			if !selection.Obj().Exported() {
				continue
			}
			w.walk(selection.Type(), append(path, "method "+selection.Obj().Name()), depth+1)
		}
	}
}

// blockingReason explains why one candidate owned type cannot cross the public
// boundary.
//
// Every candidate owned type that reaches the boundary blocks. The approved
// design admits no exception: a candidate "cannot own a defined type, interface
// method type, or function type that crosses the generated public boundary",
// and a named interface is a defined type like any other.
//
// An earlier version of this gate made a structural exception for interfaces,
// on the reasoning that a caller satisfies an interface by shape rather than by
// name, so a relocated copy would still accept the consumer's implementations.
// That reasoning is true and insufficient. It only covers the direction where
// the consumer implements the interface; it says nothing about the direction
// where the consumer already holds a value of the real interface type and has
// to pass it in, which is a nominal assignment and fails. It also assumes the
// interface's whole future is structural, when adding one unexported method
// upstream would seal it and silently invalidate every copy already published
// under an immutable tag.
//
// So the shape and the method set are still analysed, and what they produce is
// the specific reason rather than a pass. An operator reading "the transitive
// method set reaches user.Info" learns something an operator reading "interfaces
// block" does not.
func (w *interopWalker) blockingReason(named *types.Named, qualified string) string {
	iface, isInterface := named.Underlying().(*types.Interface)
	if !isInterface {
		return "Go type identity is nominal, so a relocated declaration is a different type from the one a consumer already holds"
	}
	if w.identity[qualified] {
		return "the generated module must satisfy this exact interface, so its real identity is required"
	}
	if method := unexportedMethod(iface); method != "" {
		// A sealed interface is the strongest case: only its own package can
		// implement it, because the method name is qualified by that package,
		// so a relocated copy could never be satisfied by anything at all.
		return "the interface has unexported method " + method +
			", which seals it to its declaring package, so a relocated copy could never be implemented or satisfied"
	}
	if dirty := w.methodSetOwner(iface, make(map[*types.TypeName]bool), 0); dirty != "" {
		return "the transitive method set reaches candidate owned type " + dirty + ", which would be relocated with it"
	}
	return "a relocated interface is a distinct type, so a consumer holding the real one cannot pass it across the boundary"
}

// unexportedMethod returns the first unexported method an interface requires,
// including through an embedded interface, or the empty string.
func unexportedMethod(iface *types.Interface) string {
	var names []string
	for i := range iface.NumMethods() {
		if method := iface.Method(i); !method.Exported() {
			names = append(names, method.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)
	return names[0]
}

// methodSetOwner returns the first candidate owned type the interface's
// transitive method set names, or the empty string.
func (w *interopWalker) methodSetOwner(iface *types.Interface, seen map[*types.TypeName]bool, depth int) string {
	if iface == nil || depth > maxInteropDepth {
		return ""
	}
	var found []string
	var inspect func(types.Type, int)
	inspect = func(typ types.Type, d int) {
		if typ == nil || d > maxInteropDepth {
			return
		}
		switch t := typ.(type) {
		case *types.Alias:
			inspect(types.Unalias(t), d+1)
		case *types.Named:
			object := t.Obj()
			if object == nil || object.Pkg() == nil {
				return
			}
			if w.owned[object.Pkg().Path()] {
				found = append(found, object.Pkg().Path()+"."+object.Name())
				return
			}
			if seen[object] {
				return
			}
			seen[object] = true
			inspect(t.Underlying(), d+1)
		case *types.Pointer:
			inspect(t.Elem(), d+1)
		case *types.Slice:
			inspect(t.Elem(), d+1)
		case *types.Array:
			inspect(t.Elem(), d+1)
		case *types.Chan:
			inspect(t.Elem(), d+1)
		case *types.Map:
			inspect(t.Key(), d+1)
			inspect(t.Elem(), d+1)
		case *types.Struct:
			for i := range t.NumFields() {
				inspect(t.Field(i).Type(), d+1)
			}
		case *types.Signature:
			for _, tuple := range []*types.Tuple{t.Params(), t.Results()} {
				if tuple == nil {
					continue
				}
				for i := range tuple.Len() {
					inspect(tuple.At(i).Type(), d+1)
				}
			}
		case *types.Interface:
			for i := range t.NumExplicitMethods() {
				inspect(t.ExplicitMethod(i).Type(), d+1)
			}
			for i := range t.NumEmbeddeds() {
				inspect(t.EmbeddedType(i), d+1)
			}
		}
	}

	for i := range iface.NumExplicitMethods() {
		inspect(iface.ExplicitMethod(i).Type(), depth+1)
	}
	for i := range iface.NumEmbeddeds() {
		inspect(iface.EmbeddedType(i), depth+1)
	}
	if len(found) == 0 {
		return ""
	}
	slices.Sort(found)
	return found[0]
}

// record adds a finding unless the same type was already reported.
func (w *interopWalker) record(finding interopFinding) {
	if w.reported[finding.Type] {
		return
	}
	w.reported[finding.Type] = true
	w.findings = append(w.findings, finding)
}

// position renders a declaration position, or the empty string when the file
// set does not know it.
func (w *interopWalker) position(pos token.Pos) string {
	if w.fset == nil || !pos.IsValid() {
		return ""
	}
	return w.fset.Position(pos).String()
}

// shapeOf names what a type is underneath, for the report.
func shapeOf(typ types.Type) string {
	switch typ.(type) {
	case *types.Interface:
		return "interface"
	case *types.Struct:
		return "struct"
	case *types.Basic:
		return "basic"
	case *types.Slice:
		return "slice"
	case *types.Map:
		return "map"
	case *types.Signature:
		return "func"
	case *types.Chan:
		return "chan"
	case *types.Pointer:
		return "pointer"
	case *types.Array:
		return "array"
	default:
		return "other"
	}
}

// compareInteropFindings orders findings so a report is byte stable.
func compareInteropFindings(a, b interopFinding) int {
	if c := compareStrings(a.Type, b.Type); c != 0 {
		return c
	}
	return compareStrings(a.Path, b.Path)
}
