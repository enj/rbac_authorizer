package facade

import (
	"fmt"
	"go/types"
	"strconv"
)

// leakWalk proves that nothing a consumer can reach through the published
// surface is a type a consumer cannot name.
//
// The failure it exists to prevent is quiet. A facade can alias one type,
// compile, publish, and still hand back values of a relocated type that lives
// under an internal prefix: the consumer receives the value, cannot declare a
// variable of its type, cannot implement its interface, and cannot write a
// function that takes it. Nothing in the build says so, because the module
// itself can name everything it relocated. Only a walk of the exported surface
// from outside the module's own visibility rules finds it.
//
// The walk stops at the module boundary in the other direction too. A named
// type from a real dependency is reachable, nameable, and already public, so it
// is left exactly as it is and its members are not descended into: they are
// that module's API, not this one's. What is descended into is its type
// arguments, because a generic external type instantiated with a relocated one
// puts an unnameable type back into the surface through the side door.
type leakWalk struct {
	// internal reports whether a consumer of the generated module is unable to
	// import a package, which is what makes a type declared there unnameable.
	internal func(pkgPath string) bool
	// aliases holds the declarations the facade publishes.
	aliases map[*types.TypeName]string
	// seen bounds the walk. Every go/types type is a pointer, so identity is
	// the right key, and a recursive type is visited once.
	seen map[types.Type]bool
}

// newLeakWalk starts a walk over one published surface.
func newLeakWalk(spec Spec, aliases map[*types.TypeName]string) *leakWalk {
	return &leakWalk{internal: spec.unnameable, aliases: aliases, seen: make(map[types.Type]bool)}
}

// walk descends one type, reporting the first reachable internal type that the
// facade does not publish.
//
// The trail names the route that reached the type. A leak is usually several
// hops from the export that caused it, and "internal type X is reachable" on
// its own leaves the reader to search the surface for the path; the trail turns
// the fix into reading one line.
func (w *leakWalk) walk(t types.Type, trail string) error {
	if t == nil || w.seen[t] {
		return nil
	}
	w.seen[t] = true

	switch t := t.(type) {
	case *types.Basic:
		return nil
	case *types.TypeParam:
		return w.walk(t.Constraint(), trail+" -> constraint of "+t.Obj().Name())
	case *types.Alias:
		return w.declared(t.Obj(), t.TypeArgs(), types.Unalias(t), trail)
	case *types.Named:
		return w.declared(t.Obj(), t.TypeArgs(), t, trail)
	case *types.Pointer:
		return w.walk(t.Elem(), trail)
	case *types.Slice:
		return w.walk(t.Elem(), trail+" -> element")
	case *types.Array:
		return w.walk(t.Elem(), trail+" -> element")
	case *types.Chan:
		return w.walk(t.Elem(), trail+" -> element")
	case *types.Map:
		if err := w.walk(t.Key(), trail+" -> key"); err != nil {
			return err
		}
		return w.walk(t.Elem(), trail+" -> element")
	case *types.Signature:
		return w.signature(t, trail)
	case *types.Struct:
		// An unnamed struct is spelled structurally, so every field of it is
		// part of the type a consumer has to be able to write, including the
		// unexported ones they could never fill in.
		for i := range t.NumFields() {
			field := t.Field(i)
			if err := w.walk(field.Type(), trail+" -> field "+field.Name()); err != nil {
				return err
			}
		}
		return nil
	case *types.Interface:
		return w.interfaceType(t, trail)
	case *types.Union:
		for i := range t.Len() {
			if err := w.walk(t.Term(i).Type(), trail+" -> constraint term"); err != nil {
				return err
			}
		}
		return nil
	case *types.Tuple:
		for i := range t.Len() {
			if err := w.walk(t.At(i).Type(), trail); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: type %T cannot be checked for internal leaks", ErrSpec, t)
	}
}

// declared handles a reference to a declared type, which is where the module
// boundary is decided.
func (w *leakWalk) declared(obj *types.TypeName, args *types.TypeList, resolved types.Type, trail string) error {
	// Type arguments are checked whichever side of the boundary the generic
	// itself is on, because they travel into the surface with it.
	for i := range args.Len() {
		if err := w.walk(args.At(i), trail+" -> type argument "+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	if facade, published := w.aliases[obj]; published {
		return w.members(resolved, "facade "+facade)
	}
	pkg := obj.Pkg()
	if pkg == nil {
		// A universe declaration such as error, any, or comparable belongs to
		// the language rather than to any module.
		return nil
	}
	if obj.IsAlias() && w.internal(pkg.Path()) {
		// An alias declares no type of its own, and this one is declared where a
		// consumer cannot see it, so the renderer resolves through it and spells
		// what it denotes. The walk follows, for the same reason and to the same
		// place: an alias to a dependency's type, to a basic type, or to an
		// unnamed type publishes nothing unnameable and is not a leak, while an
		// alias whose target is itself unnameable is still refused because the
		// walk continues into the target.
		return w.walk(resolved, trail)
	}
	if !w.internal(pkg.Path()) {
		// A dependency's type keeps its own identity and its own API, and an
		// alias a dependency exports is a spelling a consumer can write even
		// when it denotes something in that dependency's internal tree.
		return nil
	}
	return fmt.Errorf("%s reaches %s.%s: %w", trail, pkg.Path(), obj.Name(), ErrLeak)
}

// members descends the exported members of a published type.
//
// Only exported members are followed. An unexported field or method is not
// reachable from outside the package that declares it, so a relocated type
// hidden behind one is not in the published surface and requiring an alias for
// it would refuse facades that are perfectly usable.
func (w *leakWalk) members(t types.Type, trail string) error {
	named, ok := t.(*types.Named)
	if !ok {
		// A published alias may target an unnamed type, such as a map or a
		// function type, which has no members but does have components.
		return w.walk(t, trail)
	}
	// A generic type is published as a generic alias, which repeats its type
	// parameter list verbatim. A consumer therefore has to be able to name
	// every constraint in that list, exactly as it has to be able to name every
	// type in a signature.
	if params := named.Origin().TypeParams(); params != nil {
		for i := range params.Len() {
			param := params.At(i)
			if err := w.walk(param.Constraint(), trail+" -> constraint of "+param.Obj().Name()); err != nil {
				return err
			}
		}
	}
	if iface, ok := named.Underlying().(*types.Interface); ok {
		return w.interfaceType(iface, trail)
	}
	if structure, ok := named.Underlying().(*types.Struct); ok {
		for i := range structure.NumFields() {
			field := structure.Field(i)
			if !field.Exported() {
				continue
			}
			if err := w.walk(field.Type(), trail+" -> field "+field.Name()); err != nil {
				return err
			}
		}
	} else if err := w.walk(named.Underlying(), trail+" -> underlying type"); err != nil {
		return err
	}
	return w.methods(named, trail)
}

// methods descends the exported method set of a published type.
//
// The set is taken on the pointer, which is the method set a consumer holding
// an addressable value or a pointer actually has, and it includes methods
// promoted from embedded fields that the declared method list does not.
func (w *leakWalk) methods(named *types.Named, trail string) error {
	if named.TypeParams().Len() > 0 && named.TypeArgs().Len() == 0 {
		// An uninstantiated generic type has no method set to compute, so its
		// declared methods are walked directly.
		for i := range named.NumMethods() {
			method := named.Method(i)
			if !method.Exported() {
				continue
			}
			if err := w.walk(method.Type(), trail+" -> method "+method.Name()); err != nil {
				return err
			}
		}
		return nil
	}
	set := types.NewMethodSet(types.NewPointer(named))
	for i := range set.Len() {
		method := set.At(i).Obj()
		if !method.Exported() {
			continue
		}
		if err := w.walk(method.Type(), trail+" -> method "+method.Name()); err != nil {
			return err
		}
	}
	return nil
}

// interfaceType descends the exported methods and the type set of an interface.
func (w *leakWalk) interfaceType(t *types.Interface, trail string) error {
	for i := range t.NumMethods() {
		method := t.Method(i)
		if !method.Exported() {
			continue
		}
		if err := w.walk(method.Type(), trail+" -> method "+method.Name()); err != nil {
			return err
		}
	}
	for i := range t.NumEmbeddeds() {
		if err := w.walk(t.EmbeddedType(i), trail+" -> embedded"); err != nil {
			return err
		}
	}
	return nil
}

// signature descends a function type.
func (w *leakWalk) signature(sig *types.Signature, trail string) error {
	if params := sig.TypeParams(); params != nil {
		for i := range params.Len() {
			param := params.At(i)
			if err := w.walk(param.Constraint(), trail+" -> constraint of "+param.Obj().Name()); err != nil {
				return err
			}
		}
	}
	if err := w.walkTuple(sig.Params(), trail, "parameter"); err != nil {
		return err
	}
	return w.walkTuple(sig.Results(), trail, "result")
}

// walkTuple descends a parameter or result list, naming each position so a leak
// several hops deep still says which argument carried it.
func (w *leakWalk) walkTuple(t *types.Tuple, trail, role string) error {
	if t == nil {
		return nil
	}
	for i := range t.Len() {
		v := t.At(i)
		label := role + " " + strconv.Itoa(i)
		if v.Name() != "" {
			label = role + " " + v.Name()
		}
		if err := w.walk(v.Type(), trail+" -> "+label); err != nil {
			return err
		}
	}
	return nil
}
