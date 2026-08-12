// Package gostate holds the primitives that decide what counts as package
// level state in a Go package.
//
// Two analyses need this question answered. The dependency policy asks it of a
// staging package it might relocate, because relocation duplicates state
// instead of sharing it. The type policy asks it of an internal API package it
// might prune, because pruning removes whatever that state was doing. The
// answers have to agree: a variable that is global state for one and not for
// the other would mean one analysis approves exactly what the other refuses,
// and the disagreement would be invisible because both would report success.
//
// So the rules live here once rather than twice.
package gostate

import "go/types"

// ExportedGlobal reports whether a package scope object is shared state, and
// why.
//
// Every exported variable qualifies, whatever its type. A map or a slice can be
// mutated in place, but a string, an int, and a bool can all be reassigned by
// any importer, so a rule that flagged only the mutable-in-place shapes was not
// drawing a distinction about state at all. It only asked whether the value
// could be changed without rebinding it, and both kinds of change are visible
// to everyone holding the package.
//
// The reason is returned rather than left to the caller because the two shapes
// break for different reasons and an operator reading the report needs to know
// which one they are looking at.
func ExportedGlobal(object types.Object) (string, bool) {
	variable, ok := object.(*types.Var)
	if !ok || !variable.Exported() {
		return "", false
	}
	if IsSentinelError(variable.Type()) {
		return "", false
	}
	if MutableInPlace(variable.Type()) {
		return "an exported package variable is shared state, and a second copy of it never sees the writes made to the first", true
	}
	return "an exported package variable of a basic type is still rebindable by any importer, so a second copy of it never sees the writes made to the first", true
}

// IsSentinelError reports whether a type is exactly the predeclared error
// interface.
//
// This is the one documented exception to ExportedGlobal. An exported error
// created at its declaration is used for comparison rather than as state, and
// including them would bury every real finding under every package's
// ErrNotFound. The exception recognises the predeclared error interface exactly
// and nothing that merely resembles it, because "looks constant" is not a
// property the type checker can confirm.
func IsSentinelError(typ types.Type) bool {
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok {
		return false
	}
	object := named.Obj()
	return object != nil && object.Pkg() == nil && object.Name() == "error"
}

// MutableInPlace reports whether a value of the type can be changed without
// rebinding the variable holding it.
//
// It is not a test for whether something is state. Everything exported is
// state; this only separates the two reasons.
func MutableInPlace(typ types.Type) bool {
	switch types.Unalias(typ).Underlying().(type) {
	case *types.Map, *types.Slice, *types.Chan, *types.Signature, *types.Pointer, *types.Struct, *types.Interface:
		return true
	default:
		return false
	}
}
