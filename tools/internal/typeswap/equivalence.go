package typeswap

import (
	"fmt"
	"go/types"
	"reflect"
	"slices"
)

// maxShapeDepth bounds the recursive comparison.
//
// Cycles are handled by the visited pair set, so this bound only covers shapes
// that are finite but pathological, such as deeply instantiated generics.
const maxShapeDepth = 64

// analyzeMethodSets proves every retained use keeps working after the swap.
//
// Two things have to hold. Every exported method an internal type has, the
// external type must have too, with the same signature once the package
// qualifier is substituted, because a retained caller invoking a method that
// only the internal type declares would stop compiling. And every internal
// symbol a retained package names must exist in the external package at all,
// since a substitution that cannot name the replacement is not a substitution.
//
// An extra method on the external type is not a problem and is recorded as
// evidence rather than as a blocker. Growing an API is compatible; shrinking it
// is not.
func analyzeMethodSets(graph *Graph, internal, external *Package, uses usageSet) AnalysisReport {
	qualifier := substituting(internal.ImportPath, external.ImportPath)

	var evidence, blockers []string
	var paired int
	for _, name := range exportedNames(internal) {
		internalType, ok := lookupType(internal, name)
		if !ok {
			continue
		}
		externalType, ok := lookupType(external, name)
		if !ok {
			// A type with no counterpart only matters when retained code names
			// it. Upstream internal packages routinely carry types the
			// published one does not, and refusing on those would refuse every
			// real pairing.
			if uses.uses(name) {
				blockers = append(blockers, "retained code uses "+internal.ImportPath+"."+name+
					" but "+external.ImportPath+" declares no "+name)
			}
			continue
		}
		paired++

		internalMethods := methodSignatures(internalType, qualifier)
		externalMethods := methodSignatures(externalType, qualifier)
		for _, method := range slices.Sorted(mapKeys(internalMethods)) {
			externalSignature, present := externalMethods[method]
			switch {
			case !present:
				blockers = append(blockers, fmt.Sprintf("%s.%s has method %s that %s.%s does not",
					internal.ImportPath, name, method, external.ImportPath, name))
			case externalSignature != internalMethods[method]:
				blockers = append(blockers, fmt.Sprintf("%s.%s.%s has signature %s but %s.%s.%s has %s",
					internal.ImportPath, name, method, internalMethods[method],
					external.ImportPath, name, method, externalSignature))
			}
		}
		for _, method := range slices.Sorted(mapKeys(externalMethods)) {
			if _, present := internalMethods[method]; !present {
				evidence = append(evidence, fmt.Sprintf("%s.%s adds method %s, which is a compatible growth",
					external.ImportPath, name, method))
			}
		}
	}

	// Symbols that are not types matter too: a retained call to an internal
	// helper function needs a counterpart just as much as a type does.
	for _, symbol := range uses.Symbols {
		if external.Types.Scope().Lookup(symbol) == nil {
			blockers = append(blockers, "retained code uses "+internal.ImportPath+"."+symbol+
				" but "+external.ImportPath+" declares no "+symbol)
		}
	}

	if paired > 0 {
		evidence = append(evidence, fmt.Sprintf("%d exported types are declared by both packages with equivalent method sets", paired))
	} else {
		blockers = append(blockers, "no exported type is declared by both "+internal.ImportPath+" and "+external.ImportPath+
			", so there is nothing for a substitution to substitute")
	}

	return analysisReport(AnalysisMethodSets, evidence, blockers)
}

// analyzeFieldIdentity proves the paired types are the same shape on the wire.
//
// Field names, field order, field types, JSON tags, and protobuf tags are all
// compared, recursively and structurally. These types are serialized, so a
// reordered protobuf field number or a changed JSON name is a compatibility
// break that nothing in the build will catch: the module compiles, the tests
// pass, and a stored object decodes into the wrong field.
//
// The comparison is on type shape rather than on rendered names. Comparing a
// qualified name alone would report []PolicyRule and map[string]PolicyRule as
// equal, because both mention the same named type, and would do the same for
// PolicyRule and *PolicyRule. Those are different wire formats and different Go
// APIs, so every container is descended into and its kind compared.
func analyzeFieldIdentity(graph *Graph, internal, external *Package) AnalysisReport {
	comparer := &shapeComparer{
		internal:  internal.ImportPath,
		external:  external.ImportPath,
		qualifier: substituting(internal.ImportPath, external.ImportPath),
		visited:   map[typePair]bool{},
	}

	var evidence, blockers []string
	var compared int
	for _, name := range exportedNames(internal) {
		internalType, ok := lookupType(internal, name)
		if !ok {
			continue
		}
		externalType, ok := lookupType(external, name)
		if !ok {
			continue
		}
		before := len(comparer.differences)
		comparer.compare(internalType, externalType, name, 0)
		if len(comparer.differences) == before {
			compared++
		}
	}

	blockers = append(blockers, comparer.sortedDifferences()...)
	switch {
	case compared > 0:
		evidence = append(evidence, fmt.Sprintf(
			"%d paired types match recursively on field names, field order, container shape, field types, and JSON and protobuf tags", compared))
	case len(blockers) == 0:
		// Zero comparisons with zero differences is a vacuous pass: it means no
		// type was paired at all, which is not evidence that the shapes match.
		blockers = append(blockers, "no exported type is declared by both "+internal.ImportPath+" and "+external.ImportPath+
			", so no field identity was actually compared")
	}
	return analysisReport(AnalysisFieldIdentity, evidence, blockers)
}

// typePair keys one comparison in progress.
type typePair struct{ a, b types.Type }

// shapeComparer compares two types across the internal to external
// substitution.
type shapeComparer struct {
	internal, external string
	qualifier          types.Qualifier
	// visited records the pairs already under comparison. It is what makes a
	// self referential type terminate, and it is keyed by the pair of types
	// rather than by a rendered path because a struct with several fields of
	// its own type reaches the same pair by many distinct paths. Keying by path
	// revisits the pair once per path, which is exponential in the number of
	// self referencing fields and does not finish.
	visited     map[typePair]bool
	differences []string
}

// sortedDifferences returns the recorded differences, sorted and deduplicated.
func (c *shapeComparer) sortedDifferences() []string {
	slices.Sort(c.differences)
	return slices.Compact(c.differences)
}

// addf records one difference.
func (c *shapeComparer) addf(format string, args ...any) {
	c.differences = append(c.differences, fmt.Sprintf(format, args...))
}

// compare descends two types and records every structural difference.
func (c *shapeComparer) compare(a, b types.Type, path string, depth int) {
	if a == nil || b == nil {
		return
	}
	if depth > maxShapeDepth {
		// Running out of depth is not a match. Whatever lay beyond was never
		// compared, and on a proof obligation an uninspected subtree cannot
		// stand in for an identical one.
		c.addf("%s was not compared to the end: the recursion hit its depth bound, so the shapes below it are unverified", path)
		return
	}
	a, b = types.Unalias(a), types.Unalias(b)

	pair := typePair{a: a, b: b}
	if c.visited[pair] {
		return
	}
	c.visited[pair] = true

	if named, ok := a.(*types.Named); ok {
		otherNamed, otherOK := b.(*types.Named)
		if !otherOK {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		if !c.correspond(named, otherNamed) {
			c.addf("%s names %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		c.compare(named.Underlying(), otherNamed.Underlying(), path, depth+1)
		return
	}
	if _, ok := b.(*types.Named); ok {
		c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
		return
	}

	switch left := a.(type) {
	case *types.Basic:
		right, ok := b.(*types.Basic)
		if !ok || left.Kind() != right.Kind() {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
		}
	case *types.Pointer:
		right, ok := b.(*types.Pointer)
		if !ok {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		c.compare(left.Elem(), right.Elem(), path, depth+1)
	case *types.Slice:
		right, ok := b.(*types.Slice)
		if !ok {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		c.compare(left.Elem(), right.Elem(), path, depth+1)
	case *types.Array:
		right, ok := b.(*types.Array)
		if !ok {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		if left.Len() != right.Len() {
			c.addf("%s has length %d internally and %d externally", path, left.Len(), right.Len())
		}
		c.compare(left.Elem(), right.Elem(), path, depth+1)
	case *types.Map:
		right, ok := b.(*types.Map)
		if !ok {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		c.compare(left.Key(), right.Key(), path+" key", depth+1)
		c.compare(left.Elem(), right.Elem(), path+" value", depth+1)
	case *types.Chan:
		right, ok := b.(*types.Chan)
		if !ok {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		if left.Dir() != right.Dir() {
			c.addf("%s has a different channel direction internally and externally", path)
		}
		c.compare(left.Elem(), right.Elem(), path, depth+1)
	case *types.Struct:
		right, ok := b.(*types.Struct)
		if !ok {
			c.addf("%s is a struct internally and %s externally", path, c.render(b))
			return
		}
		c.compareStructs(left, right, path, depth)
	case *types.Signature:
		right, ok := b.(*types.Signature)
		if !ok {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
			return
		}
		c.compareSignatures(left, right, path, depth)
	case *types.Interface:
		right, ok := b.(*types.Interface)
		if !ok {
			c.addf("%s is an interface internally and %s externally", path, c.render(b))
			return
		}
		c.compareInterfaces(left, right, path, depth)
	default:
		if c.render(a) != c.render(b) {
			c.addf("%s is %s internally and %s externally", path, c.render(a), c.render(b))
		}
	}
}

// compareStructs compares two structs field by field, in order.
func (c *shapeComparer) compareStructs(a, b *types.Struct, path string, depth int) {
	if a.NumFields() != b.NumFields() {
		c.addf("%s has %d fields internally and %d externally", path, a.NumFields(), b.NumFields())
	}
	for i := range min(a.NumFields(), b.NumFields()) {
		internalField, externalField := a.Field(i), b.Field(i)
		fieldPath := path + "." + internalField.Name()

		if internalField.Name() != externalField.Name() {
			c.addf("%s field %d is %s internally and %s externally",
				path, i, internalField.Name(), externalField.Name())
			continue
		}
		if internalField.Embedded() != externalField.Embedded() {
			c.addf("%s is embedded on only one side", fieldPath)
		}
		if internalField.Exported() != externalField.Exported() {
			c.addf("%s is exported on only one side", fieldPath)
		}
		for _, tag := range []string{"json", "protobuf"} {
			got := reflect.StructTag(a.Tag(i)).Get(tag)
			want := reflect.StructTag(b.Tag(i)).Get(tag)
			if got != want {
				c.addf("%s has %s tag %q internally and %q externally", fieldPath, tag, got, want)
			}
		}
		c.compare(internalField.Type(), externalField.Type(), fieldPath, depth+1)
	}
}

// compareSignatures compares two function signatures.
func (c *shapeComparer) compareSignatures(a, b *types.Signature, path string, depth int) {
	if a.Variadic() != b.Variadic() {
		c.addf("%s is variadic on only one side", path)
	}
	c.compareTuples(a.Params(), b.Params(), path+" parameter", depth)
	c.compareTuples(a.Results(), b.Results(), path+" result", depth)
}

// compareTuples compares a parameter or result list.
func (c *shapeComparer) compareTuples(a, b *types.Tuple, path string, depth int) {
	lenA, lenB := 0, 0
	if a != nil {
		lenA = a.Len()
	}
	if b != nil {
		lenB = b.Len()
	}
	if lenA != lenB {
		c.addf("%s count is %d internally and %d externally", path, lenA, lenB)
	}
	for i := range min(lenA, lenB) {
		c.compare(a.At(i).Type(), b.At(i).Type(), fmt.Sprintf("%s %d", path, i), depth+1)
	}
}

// compareInterfaces compares two interfaces by their method sets.
func (c *shapeComparer) compareInterfaces(a, b *types.Interface, path string, depth int) {
	left := interfaceMethods(a)
	right := interfaceMethods(b)
	for _, name := range slices.Sorted(mapKeysOf(left)) {
		other, ok := right[name]
		if !ok {
			c.addf("%s has method %s internally and not externally", path, name)
			continue
		}
		c.compare(left[name], other, path+"."+name, depth+1)
	}
	for _, name := range slices.Sorted(mapKeysOf(right)) {
		if _, ok := left[name]; !ok {
			c.addf("%s has method %s externally and not internally", path, name)
		}
	}
}

// interfaceMethods indexes an interface's methods by name, including embedded
// ones.
func interfaceMethods(iface *types.Interface) map[string]types.Type {
	methods := make(map[string]types.Type, iface.NumMethods())
	for i := range iface.NumMethods() {
		method := iface.Method(i)
		methods[method.Name()] = method.Type()
	}
	return methods
}

// correspond reports whether two named types are the two sides of one
// declaration.
//
// They correspond when they have the same name and their packages are the two
// sides of the configured pairing, or when they are literally the same package,
// which is the case for a shared dependency such as metav1 that both API
// packages reference. A name that matches across two unrelated packages does
// not correspond, because the substitution being proved is this pairing and no
// other.
func (c *shapeComparer) correspond(a, b *types.Named) bool {
	objectA, objectB := a.Obj(), b.Obj()
	if objectA == nil || objectB == nil || objectA.Name() != objectB.Name() {
		return false
	}
	pathA, pathB := "", ""
	if objectA.Pkg() != nil {
		pathA = objectA.Pkg().Path()
	}
	if objectB.Pkg() != nil {
		pathB = objectB.Pkg().Path()
	}
	if pathA == pathB {
		return true
	}
	return pathA == c.internal && pathB == c.external
}

// render writes a type for a difference message, with the internal package
// spelled as the external one so the only differences shown are real.
func (c *shapeComparer) render(typ types.Type) string { return typeString(typ, c.qualifier) }

// methodSignatures renders one type's exported method set, keyed by name.
//
// A named interface's method set is the interface's own. Taking the pointer
// method set of an interface returns nothing, which would make every interface
// pairing compare two empty sets and pass without checking anything. Everything
// else uses the pointer method set so promoted and pointer receiver methods
// count, because a retained caller holding a *Role can invoke all of them.
func methodSignatures(named *types.Named, qualifier types.Qualifier) map[string]string {
	signatures := make(map[string]string)
	subject := types.Type(types.NewPointer(named))
	if _, isInterface := named.Underlying().(*types.Interface); isInterface {
		subject = named
	}
	methodSet := types.NewMethodSet(subject)
	for i := range methodSet.Len() {
		selection := methodSet.At(i)
		if !selection.Obj().Exported() {
			continue
		}
		signatures[selection.Obj().Name()] = typeString(selection.Type(), qualifier)
	}
	return signatures
}

// mapKeys yields a string keyed map's keys so callers can sort them.
func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// mapKeysOf yields a type keyed map's keys so callers can sort them.
func mapKeysOf(m map[string]types.Type) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}
