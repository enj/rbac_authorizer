package facade

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"
)

// importTable assigns one deterministic local name to every package the
// generated file references.
//
// Local names cannot be left to the package's own name. Two relocated packages
// can be named v1, a relocated package can be named the same as an external
// one, and a package name can collide with an identifier the facade itself
// declares. Any of those would produce a file that does not compile, or worse,
// one that compiles while a type expression resolves against the wrong package.
//
// The table is filled in two phases. A first render records which packages are
// actually referenced, then names are assigned over the recorded set sorted by
// import path, and only then is the file rendered for real. Assigning during a
// single render would make a name depend on the order declarations happened to
// be visited, so adding one export could silently renumber the aliases of
// unrelated ones and every generated file would churn.
type importTable struct {
	// reserved holds identifiers a local name may not take: the keywords, the
	// predeclared identifiers, and everything the generated file declares.
	reserved map[string]bool
	// recorded holds the packages a render referenced, keyed by import path.
	recorded map[string]*types.Package
	// local maps an import path onto its assigned local name.
	local map[string]string
	// assigned reports whether names have been assigned, which is what tells
	// the recording phase from the rendering one.
	assigned bool
}

// newImportTable starts a table whose local names avoid the given identifiers.
func newImportTable(reserved []string) *importTable {
	table := &importTable{
		reserved: make(map[string]bool, len(reserved)),
		recorded: make(map[string]*types.Package),
		local:    make(map[string]string),
	}
	for _, name := range reserved {
		table.reserved[name] = true
	}
	return table
}

// qualify records a package during the recording phase and reports its local
// name during the rendering one.
func (t *importTable) qualify(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	if !t.assigned {
		t.recorded[pkg.Path()] = pkg
		// The recorded phase's output is discarded, so any stable string will
		// do; the package's own name keeps a dry render readable in a
		// diagnostic.
		return pkg.Name()
	}
	if name, ok := t.local[pkg.Path()]; ok {
		return name
	}
	// A package referenced only in the rendering phase would mean the two
	// phases disagree, which is a generator bug rather than bad input. Naming
	// it after its path makes the resulting file fail to compile loudly instead
	// of silently resolving against whatever else is in scope.
	return "!missing_import(" + pkg.Path() + ")"
}

// assign fixes a local name for every recorded package.
func (t *importTable) assign() {
	t.assigned = true
	used := make(map[string]bool, len(t.recorded))
	for _, path := range t.paths() {
		name := t.pick(t.recorded[path], used)
		t.local[path] = name
		used[name] = true
	}
}

// paths reports the recorded import paths in sorted order, which is the order
// names are assigned in and the order the import block is written in.
func (t *importTable) paths() []string {
	paths := make([]string, 0, len(t.recorded))
	for path := range t.recorded {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// localNames reports the assigned local names as a set, which is what a
// parameter name must not shadow.
func (t *importTable) localNames() map[string]bool {
	names := make(map[string]bool, len(t.local))
	for _, name := range t.local {
		names[name] = true
	}
	return names
}

// pick chooses the local name of one package.
//
// The package's own name is preferred, because an import that needs no alias is
// the one a reader does not have to think about. When it is unavailable the
// candidates grow leftwards through the import path, so two packages named v1
// become v1 and rbac_v1 rather than v1 and v1_2, and the alias says which one it
// is. Numeric suffixes are the last resort for paths that agree all the way up.
func (t *importTable) pick(pkg *types.Package, used map[string]bool) string {
	free := func(name string) bool {
		return name != "" && name != "_" && token.IsIdentifier(name) &&
			token.Lookup(name) == token.IDENT &&
			types.Universe.Lookup(name) == nil &&
			!t.reserved[name] && !used[name]
	}
	if free(pkg.Name()) {
		return pkg.Name()
	}
	elements := strings.Split(pkg.Path(), "/")
	for i := len(elements) - 1; i >= 0; i-- {
		if candidate := sanitizeIdentifier(strings.Join(elements[i:], "_")); free(candidate) {
			return candidate
		}
	}
	base := sanitizeIdentifier(pkg.Name())
	if base == "" || !token.IsIdentifier(base) {
		base = "pkg"
	}
	for suffix := 2; ; suffix++ {
		if candidate := base + strconv.Itoa(suffix); free(candidate) {
			return candidate
		}
	}
}

// sanitizeIdentifier maps a path fragment onto something that can be an
// identifier, replacing every rune Go does not allow and prefixing a leading
// digit, which a version element such as 3scale would otherwise produce.
func sanitizeIdentifier(fragment string) string {
	var b strings.Builder
	b.Grow(len(fragment))
	for i, r := range fragment {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// render writes the import block, or nothing when the file needs none.
func (t *importTable) render() string {
	paths := t.paths()
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("import (\n")
	for _, path := range paths {
		name := t.local[path]
		// An alias equal to the package's own name is noise, and go/format
		// would keep it, so it is left out where it says nothing.
		if pkg := t.recorded[path]; pkg != nil && pkg.Name() == name {
			b.WriteString("\t" + strconv.Quote(path) + "\n")
			continue
		}
		b.WriteString("\t" + name + " " + strconv.Quote(path) + "\n")
	}
	b.WriteString(")\n")
	return b.String()
}

// renderer writes Go type expressions for the generated file.
//
// It exists instead of types.TypeString because the facade renames what it
// publishes. A relocated type has to appear in the generated signatures under
// its facade name rather than under an internal package qualifier: the two name
// the same type, so either compiles, but only the facade name is one a consumer
// can write, and only the facade name keeps an internal path out of the
// published documentation. Everything else follows go/types' own spelling
// exactly, which a test pins by rendering a corpus of types with an empty
// rename table and comparing against types.TypeString.
type renderer struct {
	// aliases maps the declaration of a relocated named type onto the facade
	// name that publishes it.
	aliases map[*types.TypeName]string
	// qualify reports how a package is named in the output. The generated file
	// uses an import table's local names; the API manifest uses full import
	// paths, because a manifest is compared across runs rather than compiled.
	qualify func(pkg *types.Package) string
	// internal reports whether a consumer of the generated module is unable to
	// import a package, which is what decides whether an alias declaration is a
	// spelling a consumer could use or one that has to be resolved through. It
	// is the same predicate the leak walk applies, so what the walk permits is
	// exactly what the renderer can spell.
	internal func(pkgPath string) bool
	// mode decides the divergences between the two things this renderer
	// produces.
	mode renderMode
}

// renderMode selects what a rendering is for.
//
// The two outputs are close enough to share a renderer and different enough
// that the differences have to be stated rather than discovered. Source has to
// compile; a manifest has to compare.
type renderMode uint8

const (
	// renderSource produces Go the generated package will compile.
	//
	// Parameter names are kept, because they are the only documentation a
	// signature carries. Anything without a source spelling is refused: an
	// untyped constant kind, and an unnamed struct or interface carrying an
	// unexported member of another package.
	renderSource renderMode = iota
	// renderManifest produces the comparison key of one API element.
	//
	// Parameter names are dropped, because Go's notion of API identity does not
	// include them: an upstream rename changes nothing a consumer can call, and
	// a manifest that reported it would fail a pre-prune against post-prune
	// comparison over a difference that is not one. Kinds with no source
	// spelling are recorded rather than refused, because a manifest describes
	// what a declaration is rather than reproducing it, and an untyped constant
	// is a real and observable part of an API.
	renderManifest
)

// typ renders a type expression.
func (r *renderer) typ(t types.Type) (string, error) {
	switch t := t.(type) {
	case *types.Basic:
		return r.basic(t)
	case *types.Alias:
		return r.alias(t)
	case *types.Named:
		return r.named(t)
	case *types.TypeParam:
		return t.Obj().Name(), nil
	case *types.Pointer:
		return r.prefixed("*", t.Elem())
	case *types.Slice:
		return r.prefixed("[]", t.Elem())
	case *types.Array:
		return r.prefixed("["+strconv.FormatInt(t.Len(), 10)+"]", t.Elem())
	case *types.Map:
		key, err := r.typ(t.Key())
		if err != nil {
			return "", err
		}
		return r.prefixed("map["+key+"]", t.Elem())
	case *types.Chan:
		return r.chanType(t)
	case *types.Signature:
		signature, err := r.signature(t)
		if err != nil {
			return "", err
		}
		return "func" + signature, nil
	case *types.Struct:
		return r.structType(t)
	case *types.Interface:
		return r.interfaceType(t)
	case *types.Union:
		return r.union(t)
	case *types.Tuple:
		// A tuple is not a type expression in Go source. Reaching one means a
		// signature was rendered through the wrong entry point.
		return "", fmt.Errorf("%w: a tuple has no source spelling", ErrSpec)
	default:
		return "", fmt.Errorf("%w: type %T has no supported spelling", ErrSpec, t)
	}
}

// basic renders a predeclared type.
func (r *renderer) basic(t *types.Basic) (string, error) {
	switch t.Kind() {
	case types.Invalid:
		return "", fmt.Errorf("%w: the public API reaches an invalid type, which means the module did not type check", ErrLoad)
	case types.UnsafePointer:
		// unsafe is a real import even though its member is a basic type.
		return r.qualify(types.Unsafe) + ".Pointer", nil
	}
	if t.Info()&types.IsUntyped != 0 && r.mode == renderSource {
		// An untyped kind has no source spelling. A facade forwards a constant
		// rather than restating its type, so reaching one here means a type was
		// rendered where the constant's own declaration was meant.
		return "", fmt.Errorf("%w: untyped %s has no source spelling", ErrSpec, t.Name())
	}
	return t.Name(), nil
}

// prefixed renders a type with a syntactic prefix such as * or [].
func (r *renderer) prefixed(prefix string, elem types.Type) (string, error) {
	rendered, err := r.typ(elem)
	if err != nil {
		return "", err
	}
	return prefix + rendered, nil
}

// named renders a reference to a defined type.
//
// An uninstantiated generic type carries its parameter list, which is how
// go/types spells it and how it reads in a declaration. It never appears in a
// type expression, where a generic type is always instantiated, so spelling it
// this way cannot produce a reference that silently means something else: it is
// either in a position where the list belongs or in one that does not compile.
func (r *renderer) named(t *types.Named) (string, error) {
	rendered, err := r.namedOrAlias(t.Obj(), t.TypeArgs())
	if err != nil {
		return "", err
	}
	if t.TypeArgs().Len() > 0 || t.TypeParams().Len() == 0 {
		return rendered, nil
	}
	params, err := r.typeParams(t.TypeParams())
	if err != nil {
		return "", err
	}
	return rendered + params, nil
}

// alias renders a reference to an alias declaration.
//
// An alias the facade does not publish and a consumer cannot import is resolved
// through rather than spelled. It is not a distinct type, its name lives where
// no importer can reach it, and rendering it would put a spelling into the
// published API that no consumer can write and that the generated file might
// not even be permitted to import. Every other alias, including one a
// dependency exports over its own internal type and a universe one such as any,
// is a perfectly good public spelling and is kept as written.
func (r *renderer) alias(t *types.Alias) (string, error) {
	obj := t.Obj()
	if _, published := r.aliases[obj]; published {
		return r.namedOrAlias(obj, t.TypeArgs())
	}
	if pkg := obj.Pkg(); pkg != nil && r.internal != nil && r.internal(pkg.Path()) {
		return r.typ(types.Unalias(t))
	}
	return r.namedOrAlias(obj, t.TypeArgs())
}

// namedOrAlias renders a reference to a declared type, using the facade name
// when the facade publishes it and a qualified reference otherwise.
func (r *renderer) namedOrAlias(obj *types.TypeName, args *types.TypeList) (string, error) {
	name, ok := r.aliases[obj]
	switch {
	case ok:
	case obj.Pkg() == nil:
		// A universe declaration such as error, any, or comparable is spelled
		// bare and imports nothing.
		name = obj.Name()
	default:
		name = r.qualify(obj.Pkg()) + "." + obj.Name()
	}
	if args == nil || args.Len() == 0 {
		return name, nil
	}
	rendered := make([]string, args.Len())
	for i := range rendered {
		argument, err := r.typ(args.At(i))
		if err != nil {
			return "", err
		}
		rendered[i] = argument
	}
	return name + "[" + strings.Join(rendered, ", ") + "]", nil
}

// chanType renders a channel, parenthesising a receive only element so the
// direction arrows cannot rebind.
func (r *renderer) chanType(t *types.Chan) (string, error) {
	var prefix string
	parenthesise := false
	switch t.Dir() {
	case types.SendRecv:
		prefix = "chan "
		if elem, ok := t.Elem().(*types.Chan); ok && elem.Dir() == types.RecvOnly {
			parenthesise = true
		}
	case types.SendOnly:
		prefix = "chan<- "
	case types.RecvOnly:
		prefix = "<-chan "
	default:
		return "", fmt.Errorf("%w: channel direction %d is unknown", ErrSpec, t.Dir())
	}
	elem, err := r.typ(t.Elem())
	if err != nil {
		return "", err
	}
	if parenthesise {
		elem = "(" + elem + ")"
	}
	return prefix + elem, nil
}

// structType renders a struct literal type.
//
// An unexported field is refused rather than rendered. An unnamed struct type
// is spelled structurally, and a field name that starts with a lower case
// letter is qualified by the package that wrote it, so the same characters
// written in the generated package would declare a different type. Reproducing
// it is not possible, and rendering it anyway would produce a signature that
// looks like the upstream one while accepting nothing the upstream one accepts.
func (r *renderer) structType(t *types.Struct) (string, error) {
	fields := make([]string, 0, t.NumFields())
	for i := range t.NumFields() {
		field := t.Field(i)
		if !field.Exported() && r.mode == renderSource {
			return "", fmt.Errorf("%w: an unnamed struct carries the unexported field %s from %s",
				ErrUnrepresentable, field.Name(), packagePathOf(field))
		}
		rendered, err := r.typ(field.Type())
		if err != nil {
			return "", err
		}
		if !field.Embedded() {
			rendered = field.Name() + " " + rendered
		}
		if tag := t.Tag(i); tag != "" {
			rendered += " " + strconv.Quote(tag)
		}
		fields = append(fields, rendered)
	}
	return "struct{" + strings.Join(fields, "; ") + "}", nil
}

// anyUnderlying is the one empty interface that go/types, and therefore this
// renderer, spells as any rather than as interface{}.
//
// The distinction is by identity rather than by shape: an interface literal a
// consumer wrote as interface{} keeps that spelling, and the predeclared alias
// keeps its name. Both mean the same type, and matching go/types here is what
// lets a single test pin the rest of the spelling against it.
var anyUnderlying = types.Universe.Lookup("any").Type().Underlying()

// interfaceType renders an interface literal type from its written form, so an
// embedded interface stays embedded rather than being flattened into the
// methods it contributes.
func (r *renderer) interfaceType(t *types.Interface) (string, error) {
	if t == anyUnderlying {
		return "any", nil
	}
	parts := make([]string, 0, t.NumExplicitMethods()+t.NumEmbeddeds())
	for i := range t.NumExplicitMethods() {
		method := t.ExplicitMethod(i)
		if !method.Exported() && r.mode == renderSource {
			// An unexported method of an unnamed interface is qualified by the
			// package that declared it, so no other package can write the same
			// interface or implement it.
			return "", fmt.Errorf("%w: an unnamed interface carries the unexported method %s from %s",
				ErrUnrepresentable, method.Name(), packagePathOf(method))
		}
		signature, ok := method.Type().(*types.Signature)
		if !ok {
			return "", fmt.Errorf("%w: interface method %s is not a function", ErrSpec, method.Name())
		}
		rendered, err := r.signature(signature)
		if err != nil {
			return "", err
		}
		parts = append(parts, method.Name()+rendered)
	}
	for i := range t.NumEmbeddeds() {
		rendered, err := r.typ(t.EmbeddedType(i))
		if err != nil {
			return "", err
		}
		parts = append(parts, rendered)
	}
	return "interface{" + strings.Join(parts, "; ") + "}", nil
}

// union renders a constraint's type set.
func (r *renderer) union(t *types.Union) (string, error) {
	terms := make([]string, t.Len())
	for i := range terms {
		term := t.Term(i)
		rendered, err := r.typ(term.Type())
		if err != nil {
			return "", err
		}
		if term.Tilde() {
			rendered = "~" + rendered
		}
		terms[i] = rendered
	}
	return strings.Join(terms, " | "), nil
}

// signature renders the parameter and result lists of a function type,
// including the leading type parameter list when it has one.
func (r *renderer) signature(sig *types.Signature) (string, error) {
	var b strings.Builder
	params, err := r.typeParams(sig.TypeParams())
	if err != nil {
		return "", err
	}
	b.WriteString(params)
	rendered, err := r.tuple(sig.Params(), sig.Variadic())
	if err != nil {
		return "", err
	}
	b.WriteString("(" + rendered + ")")

	results := sig.Results()
	if results.Len() == 0 {
		return b.String(), nil
	}
	rendered, err = r.tuple(results, false)
	if err != nil {
		return "", err
	}
	if results.Len() == 1 && (r.mode != renderSource || results.At(0).Name() == "") {
		return b.String() + " " + rendered, nil
	}
	return b.String() + " (" + rendered + ")", nil
}

// typeParams renders a type parameter list, or nothing when there is none.
func (r *renderer) typeParams(list *types.TypeParamList) (string, error) {
	if list == nil || list.Len() == 0 {
		return "", nil
	}
	parts := make([]string, list.Len())
	for i := range parts {
		param := list.At(i)
		constraint, err := r.typ(param.Constraint())
		if err != nil {
			return "", err
		}
		parts[i] = param.Obj().Name() + " " + constraint
	}
	return "[" + strings.Join(parts, ", ") + "]", nil
}

// tuple renders a parameter or result list, keeping the declared names because
// they are what a consumer reads in the published documentation.
func (r *renderer) tuple(t *types.Tuple, variadic bool) (string, error) {
	parts := make([]string, t.Len())
	for i := range parts {
		v := t.At(i)
		rendered, err := r.variadicAware(v.Type(), variadic && i == t.Len()-1)
		if err != nil {
			return "", err
		}
		if r.mode == renderSource && v.Name() != "" {
			rendered = v.Name() + " " + rendered
		}
		parts[i] = rendered
	}
	return strings.Join(parts, ", "), nil
}

// packagePathOf names the package an object belongs to, for a diagnostic about
// an object the generated package cannot reproduce.
func packagePathOf(object types.Object) string {
	if pkg := object.Pkg(); pkg != nil {
		return pkg.Path()
	}
	return "the universe scope"
}

// variadicAware renders a parameter type, spelling the final parameter of a
// variadic function as ...T rather than as the []T go/types records.
//
// The distinction is not cosmetic. A forwarding declaration that spelled the
// final parameter []T would be a different function: callers would have to
// build the slice themselves, and every existing call site would stop
// compiling.
func (r *renderer) variadicAware(t types.Type, final bool) (string, error) {
	if !final {
		return r.typ(t)
	}
	slice, ok := t.(*types.Slice)
	if !ok {
		return "", fmt.Errorf("%w: the final parameter of a variadic function is %s rather than a slice", ErrSpec, t)
	}
	return r.prefixed("...", slice.Elem())
}
