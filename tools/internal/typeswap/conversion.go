package typeswap

import (
	"go/ast"
	"go/types"
	"slices"
	"strings"
)

// conversionPrefix and conversionInfix are the generated naming convention,
// for example Convert_v1_Role_To_rbac_Role.
const (
	conversionPrefix = "Convert_"
	// autoConversionPrefix is the inner body conversion-gen emits alongside the
	// exported wrapper, which the wrapper then calls.
	autoConversionPrefix = "autoConvert_"
	conversionInfix      = "_To_"
)

// conversionFunc is one generated conversion function.
type conversionFunc struct {
	// Package is the package declaring it.
	Package string
	// Name is the function name.
	Name string
	// From and To are the struct types it converts between, or nil when the
	// signature is not the generated shape.
	From *types.Struct
	To   *types.Struct
	// ToName is the qualified name of the output type, for evidence.
	ToName string
	// Decl is the parsed declaration.
	Decl *ast.FuncDecl
	// Position locates it.
	Position string
}

// analyzeConversions proves the generated conversions are mechanical.
//
// The question is not whether the conversions work. Upstream tests them. The
// question is whether they are field preserving, because that is what decides
// whether one type can stand in for the other. A conversion that copies every
// field is evidence that the two declarations are the same shape wearing
// different package names. A conversion with a loop that reshapes a value, a
// conditional that drops one, or a call into a helper is evidence that they are
// not, however similar their field lists look, and no amount of matching field
// names makes such a substitution safe.
//
// So the body of every generated conversion is classified statement by
// statement, and anything that is not an assignment, a cast, an unsafe
// reinterpretation, a nested conversion, or the error check around one is a
// manual logic block that refuses the proof.
func analyzeConversions(graph *Graph, pair Pair) AnalysisReport {
	functions := collectConversions(graph, pair)

	var evidence, blockers []string
	if len(functions) == 0 {
		// No conversions is not a pass. Either upstream generated none, in
		// which case there is no mechanical evidence that the two types match,
		// or the file holding them was not loaded, in which case the proof was
		// never run. Both are reasons to refuse rather than to proceed.
		blockers = append(blockers, "no generated conversion functions between "+pair.Internal+" and "+pair.External+
			" were found in the graph, so there is no mechanical evidence that the two declarations match")
		return analysisReport(AnalysisConversions, evidence, blockers)
	}

	for _, function := range functions {
		manual := manualLogic(graph, function)
		for _, block := range manual {
			blockers = append(blockers, function.Name+" contains hand written logic at "+block.Position+": "+block.Detail)
		}
		if len(manual) > 0 {
			continue
		}

		pkg, ok := graph.lookup(function.Package)
		if !ok {
			blockers = append(blockers, function.Name+" is declared by a package that is not in the graph")
			continue
		}
		missing := unassignedFields(pkg, function)
		if len(missing) > 0 {
			blockers = append(blockers, function.Name+" does not assign "+strings.Join(missing, ", ")+
				" of "+function.ToName+", so the conversion is lossy and the declarations are not the same shape")
			continue
		}
		evidence = append(evidence, function.Name+" is field preserving: every field of "+function.ToName+
			" is assigned by a direct copy, a cast, an unsafe reinterpretation, or a nested conversion")
	}

	return analysisReport(AnalysisConversions, evidence, blockers)
}

// collectConversions finds every generated conversion between the paired
// packages, sorted by name.
func collectConversions(graph *Graph, pair Pair) []conversionFunc {
	var functions []conversionFunc
	for _, pkg := range graph.Packages {
		if pkg.Info == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Name == nil {
					continue
				}
				if !strings.HasPrefix(fn.Name.Name, conversionPrefix) || !strings.Contains(fn.Name.Name, conversionInfix) {
					continue
				}
				function, ok := describeConversion(graph, pkg, fn, pair)
				if !ok {
					continue
				}
				functions = append(functions, function)
			}
		}
	}
	slices.SortFunc(functions, func(a, b conversionFunc) int {
		if c := compareStrings(a.Package, b.Package); c != 0 {
			return c
		}
		return compareStrings(a.Name, b.Name)
	})
	return functions
}

// describeConversion resolves one conversion's input and output struct types.
//
// Only conversions that touch both sides of the configured pair are described.
// A generated file usually holds conversions for several API groups, and the
// ones for other groups say nothing about this pairing.
func describeConversion(graph *Graph, pkg *Package, fn *ast.FuncDecl, pair Pair) (conversionFunc, bool) {
	object, ok := pkg.Info.Defs[fn.Name].(*types.Func)
	if !ok {
		return conversionFunc{}, false
	}
	signature, ok := object.Type().(*types.Signature)
	if !ok || signature.Params().Len() < 2 {
		return conversionFunc{}, false
	}

	from, fromPath, ok := pointedStruct(signature.Params().At(0).Type())
	if !ok {
		return conversionFunc{}, false
	}
	to, toPath, ok := pointedStruct(signature.Params().At(1).Type())
	if !ok {
		return conversionFunc{}, false
	}

	touched := map[string]bool{fromPath: true, toPath: true}
	if !touched[pair.Internal] || !touched[pair.External] {
		return conversionFunc{}, false
	}

	return conversionFunc{
		Package:  pkg.ImportPath,
		Name:     fn.Name.Name,
		From:     from,
		To:       to,
		ToName:   toPath + "." + namedName(signature.Params().At(1).Type()),
		Decl:     fn,
		Position: graph.position(fn.Pos()),
	}, true
}

// pointedStruct resolves a *T parameter to T's underlying struct and package.
func pointedStruct(typ types.Type) (*types.Struct, string, bool) {
	pointer, ok := types.Unalias(typ).(*types.Pointer)
	if !ok {
		return nil, "", false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return nil, "", false
	}
	structure, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, "", false
	}
	return structure, named.Obj().Pkg().Path(), true
}

// namedName returns the name of a *T parameter's element type.
func namedName(typ types.Type) string {
	pointer, ok := types.Unalias(typ).(*types.Pointer)
	if !ok {
		return ""
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	if !ok || named.Obj() == nil {
		return ""
	}
	return named.Obj().Name()
}

// logicBlock is one statement that is not mechanical.
type logicBlock struct {
	Detail   string
	Position string
}

// manualLogic returns the statements in a conversion body that are not
// mechanical, in source order.
func manualLogic(graph *Graph, function conversionFunc) []logicBlock {
	if function.Decl.Body == nil {
		return []logicBlock{{Detail: "the function has no body", Position: function.Position}}
	}
	pkg, ok := graph.lookup(function.Package)
	if !ok || pkg.Info == nil {
		// Without type information a call cannot be told from a conversion, and
		// guessing from the parse tree alone is what let a helper call pass as
		// mechanical. An unanalysable body is refused rather than assumed
		// clean.
		return []logicBlock{{
			Detail:   "the declaring package carries no type information, so a helper call cannot be distinguished from a type conversion",
			Position: function.Position,
		}}
	}
	var blocks []logicBlock
	for _, stmt := range function.Decl.Body.List {
		if detail, mechanical := classifyStatement(pkg, stmt); !mechanical {
			blocks = append(blocks, logicBlock{Detail: detail, Position: graph.position(stmt.Pos())})
		}
	}
	return blocks
}

// classifyStatement reports whether one statement is mechanical, and why not.
//
// The allowed shapes are exactly what conversion-gen emits. Everything else is
// refused rather than inspected further, because the point of the proof is that
// the conversion required no judgement, and a statement this classifier has to
// reason about is by definition one that did.
func classifyStatement(pkg *Package, stmt ast.Stmt) (string, bool) {
	switch s := stmt.(type) {
	case *ast.ReturnStmt:
		return classifyReturn(pkg, s)
	case *ast.AssignStmt:
		return classifyAssign(pkg, s)
	case *ast.ExprStmt:
		if call, ok := ast.Unparen(s.X).(*ast.CallExpr); ok && isConversionCall(pkg, call) {
			return "", true
		}
		return "a call that is not a nested generated conversion", false
	case *ast.IfStmt:
		return classifyIf(pkg, s)
	case *ast.DeclStmt:
		return "", true
	case *ast.EmptyStmt:
		return "", true
	case *ast.RangeStmt:
		return "a range loop, which reshapes rather than copies", false
	case *ast.ForStmt:
		return "a loop, which reshapes rather than copies", false
	case *ast.SwitchStmt, *ast.TypeSwitchStmt:
		return "a switch, which chooses between values rather than copying one", false
	default:
		return "a statement shape that generated conversions do not emit", false
	}
}

// classifyReturn allows only "return nil" and a returned nested conversion.
func classifyReturn(pkg *Package, stmt *ast.ReturnStmt) (string, bool) {
	if len(stmt.Results) == 0 {
		return "", true
	}
	if len(stmt.Results) != 1 {
		return "a multiple value return, which generated conversions do not emit", false
	}
	switch result := ast.Unparen(stmt.Results[0]).(type) {
	case *ast.Ident:
		if result.Name == "nil" || result.Name == "err" {
			return "", true
		}
		return "a returned value other than nil or err", false
	case *ast.CallExpr:
		if isConversionCall(pkg, result) {
			return "", true
		}
		return "a returned call that is not a nested generated conversion", false
	default:
		return "a returned expression that generated conversions do not emit", false
	}
}

// classifyAssign allows a field copy, a cast, an unsafe reinterpretation, and
// the error assignment of a nested conversion.
func classifyAssign(pkg *Package, stmt *ast.AssignStmt) (string, bool) {
	if len(stmt.Rhs) != 1 {
		return "an assignment with several right hand values", false
	}
	if call, ok := ast.Unparen(stmt.Rhs[0]).(*ast.CallExpr); ok && isConversionCall(pkg, call) {
		return "", true
	}
	if len(stmt.Lhs) != 1 {
		return "an assignment to several destinations", false
	}
	if !isFieldOrIdent(stmt.Lhs[0]) {
		return "an assignment to something other than a field of the output", false
	}
	if detail, ok := classifyValue(pkg, stmt.Rhs[0]); !ok {
		return detail, false
	}
	return "", true
}

// classifyValue allows the right hand shapes conversion-gen emits.
func classifyValue(pkg *Package, expr ast.Expr) (string, bool) {
	switch value := ast.Unparen(expr).(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.BasicLit:
		return "", true
	case *ast.UnaryExpr:
		return classifyValue(pkg, value.X)
	case *ast.StarExpr:
		return classifyValue(pkg, value.X)
	case *ast.IndexExpr:
		return classifyValue(pkg, value.X)
	case *ast.CompositeLit:
		// An empty composite literal initialises a field; a populated one
		// builds a value that did not come from the input.
		if len(value.Elts) == 0 {
			return "", true
		}
		return "a composite literal that builds a value not copied from the input", false
	case *ast.CallExpr:
		return classifyCall(pkg, value)
	case *ast.TypeAssertExpr:
		return "a type assertion, which can fail and therefore is not a copy", false
	case *ast.BinaryExpr:
		return "an expression that computes a value rather than copying one", false
	default:
		return "a value shape that generated conversions do not emit", false
	}
}

// classifyCall is where a helper call is separated from a conversion.
//
// The distinction is made through the type checker, never through the shape of
// the callee. T(x) and sanitize(x) parse identically: both are a call whose
// callee is a bare identifier with one argument. Only types.Info knows that the
// first names a type and the second names a function, and reading the parse
// tree alone is exactly what let `out.Kind = sanitize(in.Kind)` pass as a
// mechanical copy.
func classifyCall(pkg *Package, call *ast.CallExpr) (string, bool) {
	if isConversionCall(pkg, call) {
		return "", true
	}
	if pkg.Info == nil {
		return "a call that cannot be resolved without type information", false
	}
	// A conversion's callee is a type expression. This covers T(x), (*T)(x),
	// []T(x), and unsafe.Pointer(x) alike, so the unsafe reinterpretation shape
	// needs no separate rule.
	if tv, ok := pkg.Info.Types[call.Fun]; ok && tv.IsType() {
		// A conversion has exactly one operand. Anything else is a malformed
		// tree rather than a conversion, and indexing it blindly would panic on
		// input this package is supposed to judge rather than crash on.
		if len(call.Args) != 1 {
			return "a conversion with an unexpected operand count", false
		}
		if detail, ok := classifyValue(pkg, call.Args[0]); !ok {
			return detail, false
		}
		return "", true
	}
	name := calleeName(call)
	if name == "" {
		name = "an unresolved call"
	}
	return "a call to " + name + ", which is hand written logic rather than a conversion", false
}

// classifyIf allows only the generated error check around a nested conversion.
func classifyIf(pkg *Package, stmt *ast.IfStmt) (string, bool) {
	if stmt.Else != nil {
		return "a conditional with an else branch, which chooses between values", false
	}
	if stmt.Init != nil {
		assign, ok := stmt.Init.(*ast.AssignStmt)
		if !ok {
			return "a conditional whose initialiser is not a nested conversion", false
		}
		if detail, mechanical := classifyAssign(pkg, assign); !mechanical {
			return detail, false
		}
	}
	binary, ok := ast.Unparen(stmt.Cond).(*ast.BinaryExpr)
	if !ok || !isErrorCheck(binary) {
		return "a conditional that tests something other than a conversion error", false
	}
	for _, inner := range stmt.Body.List {
		ret, ok := inner.(*ast.ReturnStmt)
		if !ok {
			return "a conditional body that does something other than return the error", false
		}
		if detail, mechanical := classifyReturn(pkg, ret); !mechanical {
			return detail, false
		}
	}
	return "", true
}

// isErrorCheck reports whether a condition is "err != nil".
func isErrorCheck(binary *ast.BinaryExpr) bool {
	left, leftOK := ast.Unparen(binary.X).(*ast.Ident)
	right, rightOK := ast.Unparen(binary.Y).(*ast.Ident)
	return leftOK && rightOK && left.Name == "err" && right.Name == "nil"
}

// isConversionCall reports whether a call invokes a generated conversion.
//
// The name convention is checked and then confirmed against the type checker,
// so a hand written function opportunistically named Convert_a_To_b is still a
// function call rather than a free pass. conversion-gen emits both the exported
// Convert_ wrapper and the autoConvert_ body, and a generated function calls the
// latter, so both spellings are recognised.
func isConversionCall(pkg *Package, call *ast.CallExpr) bool {
	name := calleeName(call)
	base := name
	if index := strings.LastIndex(base, "."); index >= 0 {
		base = base[index+1:]
	}
	if !strings.HasPrefix(base, conversionPrefix) && !strings.HasPrefix(base, autoConversionPrefix) {
		return false
	}
	if !strings.Contains(base, conversionInfix) {
		return false
	}
	if pkg.Info == nil {
		return false
	}
	// A generated conversion is a function, not a type. Confirming that here
	// keeps a type named Convert_a_To_b from being read as a nested call.
	object := calledObject(pkg.Info, call)
	_, isFunc := object.(*types.Func)
	return isFunc
}

// calledObject resolves the function a call expression invokes.
func calledObject(info *types.Info, call *ast.CallExpr) types.Object {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		return info.Uses[fun]
	case *ast.SelectorExpr:
		return info.Uses[fun.Sel]
	default:
		return nil
	}
}

// calleeName renders a call's callee as a dotted name.
func calleeName(call *ast.CallExpr) string {
	switch callee := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		return callee.Name
	case *ast.SelectorExpr:
		if ident, ok := ast.Unparen(callee.X).(*ast.Ident); ok {
			return ident.Name + "." + callee.Sel.Name
		}
		return callee.Sel.Name
	default:
		return ""
	}
}

// isFieldOrIdent reports whether an expression names a field or a local.
func isFieldOrIdent(expr ast.Expr) bool {
	switch target := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return true
	case *ast.StarExpr:
		return isFieldOrIdent(target.X)
	case *ast.IndexExpr:
		return isFieldOrIdent(target.X)
	default:
		return false
	}
}

// unassignedFields returns the output fields the conversion never writes,
// sorted.
//
// This is the field preserving half of the proof. A body made only of
// mechanical statements can still be lossy by simply not mentioning a field,
// and a dropped field is the exact failure this analysis exists to catch: it
// compiles, it passes, and it silently discards data.
func unassignedFields(pkg *Package, function conversionFunc) []string {
	assigned := make(map[string]bool)
	ast.Inspect(function.Decl.Body, func(node ast.Node) bool {
		switch expr := node.(type) {
		case *ast.AssignStmt:
			for _, target := range expr.Lhs {
				if name, ok := outputField(target); ok {
					assigned[name] = true
				}
			}
		case *ast.CallExpr:
			// A nested conversion writes through its second argument, so
			// Convert_a_To_b(&in.Rules, &out.Rules, s) assigns Rules.
			if !isConversionCall(pkg, expr) || len(expr.Args) < 2 {
				return true
			}
			if name, ok := outputField(expr.Args[1]); ok {
				assigned[name] = true
			}
		}
		return true
	})

	var missing []string
	for i := range function.To.NumFields() {
		field := function.To.Field(i)
		if !assigned[field.Name()] {
			missing = append(missing, field.Name())
		}
	}
	slices.Sort(missing)
	return missing
}

// outputField returns the field name an expression writes on the output.
func outputField(expr ast.Expr) (string, bool) {
	switch target := ast.Unparen(expr).(type) {
	case *ast.SelectorExpr:
		if ident, ok := ast.Unparen(target.X).(*ast.Ident); ok && ident.Name == "out" {
			return target.Sel.Name, true
		}
		return "", false
	case *ast.UnaryExpr:
		return outputField(target.X)
	case *ast.StarExpr:
		return outputField(target.X)
	case *ast.IndexExpr:
		return outputField(target.X)
	default:
		return "", false
	}
}
