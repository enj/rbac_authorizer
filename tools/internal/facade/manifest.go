package facade

import (
	"fmt"
	"go/constant"
	"go/types"
	"slices"
	"strings"
)

// MemberKind distinguishes the exported members a published type carries.
type MemberKind uint8

const (
	// MemberField is an exported struct field.
	MemberField MemberKind = iota
	// MemberMethod is an exported method, including one promoted from an
	// embedded field and one contributed by an embedded interface.
	MemberMethod
)

// String renders the member kind.
func (k MemberKind) String() string {
	if k == MemberField {
		return "field"
	}
	return "method"
}

// Member is one exported member of a published type.
type Member struct {
	// Name is the member's identifier.
	Name string
	// Kind is what the member is.
	Kind MemberKind
	// Type is the member's type, spelled with full package paths and without
	// parameter names. For a method it is the signature without the receiver,
	// which is what a consumer calls.
	Type string
}

// Entry is one published declaration.
type Entry struct {
	// Name is the identifier the generated package declares.
	Name string
	// Kind is the declaration that was generated.
	Kind Kind
	// Target is the relocated qualified symbol the entry forwards to, spelled
	// with the generated module's own path.
	Target string
	// Type is the declared type, spelled with full package paths and without
	// parameter names.
	Type string
	// Underlying is the underlying type of a published defined type, spelled
	// the same way.
	//
	// It is recorded because the qualified name alone hides a real API change:
	// a defined type whose underlying type moves from int to int64, or whose
	// function type gains a parameter, keeps its name and its members while
	// becoming something a consumer's existing code no longer converts to or
	// calls. It is left empty for a struct or an interface, where Members is
	// the more precise record and the underlying spelling would only repeat it
	// while also dragging in unexported fields that are not API.
	Underlying string
	// Value is a constant's exact value. It is empty for every other kind,
	// because a constant whose value changes is an API change that no type
	// string would show.
	Value string
	// Members are the exported members of a published type, sorted. They are
	// recorded because the manifest exists to detect a change in the published
	// surface, and a struct that loses a field or an interface whose method
	// changes signature is exactly such a change while its type string stays
	// the same.
	Members []Member
}

// Manifest is the deterministic description of the published API.
//
// It is the artefact that makes a change to the public surface reviewable. Two
// manifests taken across a prune, an upstream bump, or a profile edit compare
// exactly, so a removal, a signature change, or a new name is a diff rather
// than something a reader has to notice. Everything in it is spelled with full
// package paths, so a type that moved between packages while keeping its name
// does not compare equal to the type it replaced.
type Manifest struct {
	// Module is the generated module path.
	Module string
	// Package is the generated root package name.
	Package string
	// Entries are the published declarations, sorted by name.
	Entries []Entry
}

// manifestRenderer spells types the way a manifest compares them.
//
// Full import paths rather than local aliases, because a manifest is compared
// across runs rather than compiled, and a type that moved between packages
// while keeping its name must not compare equal to the one it replaced. No
// facade renames, because the manifest records what the published name denotes
// rather than what it is called. And no parameter names, because Go's notion of
// API identity does not include them: an upstream parameter rename changes
// nothing a consumer can call, and a manifest that reported it would fail the
// pre-prune against post-prune comparison for a change that is not one.
func manifestRenderer() *renderer {
	return &renderer{
		aliases: map[*types.TypeName]string{},
		qualify: func(pkg *types.Package) string { return pkg.Path() },
		mode:    renderManifest,
	}
}

// buildManifest records the published surface of a bound specification.
func buildManifest(spec Spec, exports []boundExport) (Manifest, error) {
	render := manifestRenderer()
	manifest := Manifest{Module: spec.ModulePath, Package: spec.Package, Entries: make([]Entry, 0, len(exports))}
	for _, export := range exports {
		declared, err := render.typ(export.object.Type())
		if err != nil {
			return Manifest{}, fmt.Errorf("export %s: %w", export.Name, err)
		}
		underlying, err := underlyingOf(render, export.object)
		if err != nil {
			return Manifest{}, fmt.Errorf("export %s: %w", export.Name, err)
		}
		members, err := membersOf(render, export.object)
		if err != nil {
			return Manifest{}, fmt.Errorf("export %s: %w", export.Name, err)
		}
		manifest.Entries = append(manifest.Entries, Entry{
			Name:       export.Name,
			Kind:       export.Kind,
			Target:     export.Package + "." + export.Symbol,
			Type:       declared,
			Underlying: underlying,
			Value:      constantValue(export.object),
			Members:    members,
		})
	}
	// Exports arrive sorted by facade name and collisions are already refused,
	// so the manifest is in its canonical order without sorting again.
	return manifest, nil
}

// underlyingOf records the underlying type of a published defined type.
//
// A struct and an interface are left out: Members already records their
// exported surface precisely, and the structural spelling would repeat it while
// adding unexported fields that no consumer can reach.
func underlyingOf(render *renderer, object types.Object) (string, error) {
	name, ok := object.(*types.TypeName)
	if !ok {
		return "", nil
	}
	underlying := name.Type().Underlying()
	switch underlying.(type) {
	case *types.Struct, *types.Interface:
		return "", nil
	}
	return render.typ(underlying)
}

// constantValue renders a constant's exact value, and nothing for every other
// object.
//
// ExactString is used rather than String because it never abbreviates: a large
// integer or a long string would otherwise render as a truncated form, and two
// different values could compare equal in the manifest.
func constantValue(object types.Object) string {
	konst, ok := object.(*types.Const)
	if !ok || konst.Val() == nil || konst.Val().Kind() == constant.Unknown {
		return ""
	}
	return konst.Val().ExactString()
}

// membersOf records the exported members of a published type.
func membersOf(render *renderer, object types.Object) ([]Member, error) {
	name, ok := object.(*types.TypeName)
	if !ok {
		return nil, nil
	}
	var members []Member
	if structure, ok := name.Type().Underlying().(*types.Struct); ok {
		for i := range structure.NumFields() {
			field := structure.Field(i)
			if !field.Exported() {
				continue
			}
			rendered, err := render.typ(field.Type())
			if err != nil {
				return nil, err
			}
			members = append(members, Member{Name: field.Name(), Kind: MemberField, Type: rendered})
		}
	}
	methods, err := methodMembers(render, name.Type())
	if err != nil {
		return nil, err
	}
	members = append(members, methods...)
	slices.SortFunc(members, compareMembers)
	return slices.CompactFunc(members, func(a, b Member) bool { return compareMembers(a, b) == 0 }), nil
}

// methodMembers records the exported method set of a type.
//
// An interface's method set is read from the interface itself, because a
// pointer to an interface has no methods at all. Everything else is read
// through a pointer, which is the method set a consumer holding a pointer or an
// addressable value has, and which includes methods promoted from embedded
// fields.
func methodMembers(render *renderer, t types.Type) ([]Member, error) {
	collect := func(methods []types.Object) ([]Member, error) {
		members := make([]Member, 0, len(methods))
		for _, method := range methods {
			if !method.Exported() {
				continue
			}
			member, err := methodMember(render, method)
			if err != nil {
				return nil, err
			}
			members = append(members, member)
		}
		return members, nil
	}
	if iface, ok := t.Underlying().(*types.Interface); ok {
		methods := make([]types.Object, iface.NumMethods())
		for i := range methods {
			methods[i] = iface.Method(i)
		}
		return collect(methods)
	}
	if named, ok := t.(*types.Named); ok && named.TypeParams().Len() > 0 && named.TypeArgs().Len() == 0 {
		// An uninstantiated generic type has no computable method set, so its
		// declared methods are recorded instead.
		methods := make([]types.Object, named.NumMethods())
		for i := range methods {
			methods[i] = named.Method(i)
		}
		return collect(methods)
	}
	set := types.NewMethodSet(types.NewPointer(t))
	methods := make([]types.Object, set.Len())
	for i := range methods {
		methods[i] = set.At(i).Obj()
	}
	return collect(methods)
}

// methodMember records one method without its receiver, which is not part of
// what a consumer calls and which would otherwise make the manifest of an
// aliased type mention the internal package the alias exists to hide.
func methodMember(render *renderer, method types.Object) (Member, error) {
	subject := method.Type()
	if signature, ok := subject.(*types.Signature); ok {
		subject = types.NewSignatureType(nil, nil, nil, signature.Params(), signature.Results(), signature.Variadic())
	}
	rendered, err := render.typ(subject)
	if err != nil {
		return Member{}, err
	}
	return Member{Name: method.Name(), Kind: MemberMethod, Type: rendered}, nil
}

// compareMembers orders members by name and then by kind, which is a total
// order because a type cannot have a field and a method of one name.
func compareMembers(a, b Member) int {
	if order := strings.Compare(a.Name, b.Name); order != 0 {
		return order
	}
	if a.Kind != b.Kind {
		return int(a.Kind) - int(b.Kind)
	}
	return strings.Compare(a.Type, b.Type)
}

// Render writes the manifest as deterministic text.
//
// The text form is what a report carries and what a reviewer reads, so it is
// stable rather than pretty: one field per line, a fixed field order, and
// members indented under the entry they belong to.
func (m Manifest) Render() string {
	var b strings.Builder
	b.WriteString("soapbox facade manifest\n")
	b.WriteString("module: " + m.Module + "\n")
	b.WriteString("package: " + m.Package + "\n")
	b.WriteString("\nexports:\n")
	if len(m.Entries) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, entry := range m.Entries {
		b.WriteString("  " + entry.Name + "\n")
		b.WriteString(entry.render())
	}
	return b.String()
}

// render writes one entry's body, which is also the unit a diff compares.
func (e Entry) render() string {
	var b strings.Builder
	b.WriteString("    kind: " + e.Kind.String() + "\n")
	b.WriteString("    target: " + e.Target + "\n")
	b.WriteString("    type: " + e.Type + "\n")
	if e.Underlying != "" {
		b.WriteString("    underlying: " + e.Underlying + "\n")
	}
	if e.Value != "" {
		b.WriteString("    value: " + e.Value + "\n")
	}
	for _, member := range e.Members {
		b.WriteString("    " + member.Kind.String() + " " + member.Name + " " + member.Type + "\n")
	}
	return b.String()
}

// Difference is one entry that is not the same in two manifests.
type Difference struct {
	// Name is the facade name the difference concerns, or a header field name
	// when the two manifests describe different modules or packages.
	Name string
	// Before is the rendered entry in the earlier manifest, empty when the
	// entry was added.
	Before string
	// After is the rendered entry in the later manifest, empty when the entry
	// was removed.
	After string
}

// String renders one difference as a short, greppable line pair.
func (d Difference) String() string {
	switch {
	case d.Before == "":
		return "added " + d.Name + ":\n" + d.After
	case d.After == "":
		return "removed " + d.Name + ":\n" + d.Before
	default:
		return "changed " + d.Name + ":\n  before:\n" + d.Before + "  after:\n" + d.After
	}
}

// Diff reports how two manifests differ, in a deterministic order.
//
// It is what proves the pre-prune and post-prune public API are the same. A
// prune removes files from the relocated packages, and the whole claim that a
// prune is safe rests on the published surface being unchanged by it; comparing
// the two manifests is how that claim is checked rather than asserted.
func Diff(before, after Manifest) []Difference {
	var differences []Difference
	for _, header := range []struct {
		name          string
		before, after string
	}{
		{"module", before.Module, after.Module},
		{"package", before.Package, after.Package},
	} {
		if header.before != header.after {
			differences = append(differences, Difference{Name: header.name, Before: header.before, After: header.after})
		}
	}

	names := make([]string, 0, len(before.Entries)+len(after.Entries))
	for _, entry := range before.Entries {
		names = append(names, entry.Name)
	}
	for _, entry := range after.Entries {
		names = append(names, entry.Name)
	}
	slices.Sort(names)
	names = slices.Compact(names)

	for _, name := range names {
		earlier, hadEarlier := entryNamed(before.Entries, name)
		later, hadLater := entryNamed(after.Entries, name)
		switch {
		case hadEarlier && hadLater && earlier.render() == later.render():
			continue
		case !hadEarlier:
			differences = append(differences, Difference{Name: name, After: later.render()})
		case !hadLater:
			differences = append(differences, Difference{Name: name, Before: earlier.render()})
		default:
			differences = append(differences, Difference{Name: name, Before: earlier.render(), After: later.render()})
		}
	}
	return differences
}

// entryNamed reports the entry a manifest publishes under a name.
func entryNamed(entries []Entry, name string) (Entry, bool) {
	index := slices.IndexFunc(entries, func(entry Entry) bool { return entry.Name == name })
	if index < 0 {
		return Entry{}, false
	}
	return entries[index], true
}

// Equal reports whether two manifests describe the same published API.
func (m Manifest) Equal(other Manifest) bool {
	return len(Diff(m, other)) == 0
}

// CheckAgainst refuses a published API that differs from a baseline.
//
// It is the blocking seam a caller puts between generating a facade and
// publishing it. Diff answers what changed; this answers whether the run may
// continue, and it renders every difference into the error so the decision does
// not depend on the caller also printing a report. The two uses are the same
// comparison from opposite directions: a pre-prune manifest checked against the
// post-prune one proves a prune changed no published API, and a released
// manifest checked against a regenerated one proves an upstream bump did not
// break consumers.
func (m Manifest) CheckAgainst(baseline Manifest) error {
	differences := Diff(baseline, m)
	if len(differences) == 0 {
		return nil
	}
	rendered := make([]string, len(differences))
	for i, difference := range differences {
		rendered[i] = difference.String()
	}
	return fmt.Errorf("%w:\n%s", ErrManifestChanged, strings.Join(rendered, "\n"))
}
