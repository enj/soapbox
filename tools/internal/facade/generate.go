package facade

import (
	"context"
	"fmt"
	"go/format"
	"go/token"
	"go/types"
	"slices"
	"strconv"
	"strings"

	"github.com/enj/soapbox/tools/internal/relocate"
)

// Options configures one facade generation.
type Options struct {
	// Dir is the absolute root of the generated module. It must already hold
	// the relocated packages and a go.mod the toolchain can load.
	Dir string
	// Env is the complete environment the Go toolchain runs under. It is never
	// inherited: see load for why an ambient environment would make the
	// published API depend on the shell that produced it.
	Env []string
	// Spec is the surface to publish.
	Spec Spec
}

// Result is everything one generation produced.
type Result struct {
	// Files are the generated root files, sorted by path. They are relocated
	// files with no upstream source, so a caller composes them into the file
	// set the module materializes from without a second write path.
	Files []relocate.File
	// Manifest is the published surface, for comparison against another run.
	Manifest Manifest
}

// boundExport is one export bound to the object the loaded module exports.
type boundExport struct {
	resolvedExport
	// object is the declaration the loaded module holds at the resolved symbol.
	object types.Object
}

// boundAssertion is one assertion bound to both of its types.
type boundAssertion struct {
	resolvedAssertion
	// subject is the published type the assertion is about.
	subject types.Type
	// iface is the upstream interface the subject must implement.
	iface *types.Interface
	// ifaceObj names the interface, for diagnostics and for rendering.
	ifaceObj *types.TypeName
}

// Generate produces the curated public API of a generated module.
//
// The order of the steps is the order in which a failure is cheapest to
// explain. The specification is checked on its own first, because a profile
// mistake should not cost a module load. The module is then loaded once and
// every symbol resolved against it, so an upstream removal is reported as a
// missing symbol rather than as a compile error in generated code. Only then is
// anything rendered, and the interface assertions are proved here rather than
// left for the Go compiler to discover in a file no human wrote.
func Generate(ctx context.Context, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("facade: %w", err)
	}
	exports, assertions, err := opts.Spec.validate()
	if err != nil {
		return Result{}, fmt.Errorf("facade: %w", err)
	}

	modules, err := load(ctx, opts.Dir, opts.Env, loadPatterns(exports, assertions))
	if err != nil {
		return Result{}, fmt.Errorf("facade: %w", err)
	}
	bound, err := bindExports(exports, modules)
	if err != nil {
		return Result{}, fmt.Errorf("facade: %w", err)
	}
	aliases := aliasRegistry(bound)
	if err := checkLeaks(opts.Spec, bound, aliases); err != nil {
		return Result{}, fmt.Errorf("facade: %w", err)
	}
	boundAssertions, err := bindAssertions(assertions, bound, modules)
	if err != nil {
		return Result{}, fmt.Errorf("facade: %w", err)
	}

	gen := newGenerator(opts.Spec, bound, aliases)
	facadeFile, err := gen.facadeFile()
	if err != nil {
		return Result{}, fmt.Errorf("facade %s: %w", opts.Spec.File, err)
	}
	assertionsFile, err := gen.assertionsFile(boundAssertions)
	if err != nil {
		return Result{}, fmt.Errorf("facade %s: %w", opts.Spec.AssertionsFile, err)
	}
	files := []relocate.File{
		generatedFile(opts.Spec.File, facadeFile),
		generatedFile(opts.Spec.AssertionsFile, assertionsFile),
	}
	slices.SortFunc(files, func(a, b relocate.File) int { return strings.Compare(a.Path, b.Path) })

	manifest, err := buildManifest(opts.Spec, bound)
	if err != nil {
		return Result{}, fmt.Errorf("facade manifest: %w", err)
	}
	return Result{Files: files, Manifest: manifest}, nil
}

// generatedFile wraps rendered bytes as a relocated file with no upstream
// source, which is what a generated root file is.
func generatedFile(path string, contents []byte) relocate.File {
	return relocate.File{Path: path, Mode: relocate.ModeRegular, Contents: contents, Generated: true}
}

// loadPatterns reports the packages one generation has to type check, sorted
// and without duplicates so the load is the same on every run.
func loadPatterns(exports []resolvedExport, assertions []resolvedAssertion) []string {
	patterns := make([]string, 0, len(exports)+len(assertions))
	for _, export := range exports {
		patterns = append(patterns, export.Package)
	}
	// An asserted interface lives in a dependency, and while the relocated code
	// almost certainly imports it already, loading it explicitly means the
	// assertion does not silently depend on that being true.
	for _, assertion := range assertions {
		patterns = append(patterns, assertion.Package)
	}
	slices.Sort(patterns)
	return slices.Compact(patterns)
}

// bindExports resolves every export against the loaded module and checks that
// what was found is what the profile declared.
func bindExports(exports []resolvedExport, modules loaded) ([]boundExport, error) {
	bound := make([]boundExport, 0, len(exports))
	for _, export := range exports {
		object, err := modules.scopeObject(export.Package, export.Symbol)
		if err != nil {
			return nil, fmt.Errorf("export %s (%s): %w", export.Name, export.Source, err)
		}
		if err := checkKind(export, object); err != nil {
			return nil, fmt.Errorf("export %s (%s): %w", export.Name, export.Source, err)
		}
		bound = append(bound, boundExport{resolvedExport: export, object: object})
	}
	return bound, nil
}

// checkKind refuses an upstream declaration that is not the kind the profile
// reviewed.
//
// The check is not redundant with the type checker. A struct that upstream
// turns into an interface still aliases, a constant that becomes a variable
// still has a name, and both are breaking changes to consumers that a generated
// file would absorb silently. Restating the kind in the profile is what turns
// them into a stopped run.
func checkKind(export resolvedExport, object types.Object) error {
	switch export.Kind {
	case KindType, KindInterface:
		name, ok := object.(*types.TypeName)
		if !ok {
			return fmt.Errorf("declared %s but upstream declares a %s: %w", export.Kind, objectKind(object), ErrKindMismatch)
		}
		_, isInterface := name.Type().Underlying().(*types.Interface)
		if isInterface != (export.Kind == KindInterface) {
			return fmt.Errorf("declared %s but upstream declares a %s: %w", export.Kind, objectKind(object), ErrKindMismatch)
		}
		return nil
	case KindFunc:
		fn, ok := object.(*types.Func)
		if !ok {
			return fmt.Errorf("declared func but upstream declares a %s: %w", objectKind(object), ErrKindMismatch)
		}
		signature, ok := fn.Type().(*types.Signature)
		if !ok {
			return fmt.Errorf("declared func but upstream declaration has type %s: %w", fn.Type(), ErrKindMismatch)
		}
		if signature.Recv() != nil {
			return fmt.Errorf("declared func but upstream declares a method: %w", ErrKindMismatch)
		}
		if signature.TypeParams().Len() > 0 {
			// A forwarding declaration is a different function from the one it
			// forwards to. For an ordinary function that is invisible, because
			// the signature is copied exactly. For a generic one it is not: type
			// inference at a call site runs against the declaration it sees, and
			// this package cannot prove that a re-declared type parameter list
			// infers identically in every case. Refusing loudly is the honest
			// answer; publishing a generic function whose inference quietly
			// differs from upstream's is not.
			return fmt.Errorf("%s is generic, and a forwarding declaration cannot be proved to infer identically: %w", export.Source, ErrGeneric)
		}
		return nil
	case KindConst:
		if _, ok := object.(*types.Const); !ok {
			return fmt.Errorf("declared const but upstream declares a %s: %w", objectKind(object), ErrKindMismatch)
		}
		return nil
	default:
		// A variable never reaches here: the specification refuses one before
		// anything is loaded. It is handled anyway so an upstream constant that
		// became a variable is reported as the API break it is.
		return fmt.Errorf("%w: kind %s cannot be generated", ErrSpec, export.Kind)
	}
}

// objectKind names what an object is, in the words the profile uses.
func objectKind(object types.Object) string {
	switch object := object.(type) {
	case *types.TypeName:
		if _, ok := object.Type().Underlying().(*types.Interface); ok {
			return "interface"
		}
		return "type"
	case *types.Func:
		return "func"
	case *types.Const:
		return "const"
	case *types.Var:
		return "var"
	default:
		return "declaration"
	}
}

// aliasRegistry records which declarations the facade publishes.
//
// A declaration is registered under two keys when they differ. The first is the
// symbol the profile named, which is what a signature spelled through that
// symbol resolves to. The second is the type that symbol ultimately denotes,
// because an upstream alias means two names for one type and a signature
// elsewhere in the module may well use the other one. Registering both is what
// lets the generated file spell every reachable relocated type under a facade
// name rather than under an internal package qualifier.
//
// Exports arrive sorted by facade name, so when two exports publish one type
// the earlier name wins, on every run.
func aliasRegistry(exports []boundExport) map[*types.TypeName]string {
	registry := make(map[*types.TypeName]string, len(exports))
	register := func(name *types.TypeName, facade string) {
		if _, seen := registry[name]; !seen {
			registry[name] = facade
		}
	}
	for _, export := range exports {
		name, ok := export.object.(*types.TypeName)
		if !ok {
			continue
		}
		register(name, export.Name)
		if named, ok := types.Unalias(name.Type()).(*types.Named); ok {
			register(named.Obj(), export.Name)
		}
	}
	return registry
}

// checkLeaks walks the whole published surface for relocated types a consumer
// could receive but not name.
//
// Every walk starts from the facade name rather than from the upstream one, and
// a walk that reaches another published type restarts from that name. The trail
// in a failure is then a route through the published API, which is the thing
// the reader can act on, rather than a route through the upstream package
// layout that happens to have produced it.
func checkLeaks(spec Spec, exports []boundExport, aliases map[*types.TypeName]string) error {
	walk := newLeakWalk(spec, aliases)
	for _, export := range exports {
		if err := walk.walk(export.object.Type(), "facade "+export.Name); err != nil {
			return err
		}
	}
	return nil
}

// bindAssertions resolves each asserted interface and proves the implementation
// here, where the failure can name the method that is missing.
func bindAssertions(assertions []resolvedAssertion, exports []boundExport, modules loaded) ([]boundAssertion, error) {
	bound := make([]boundAssertion, 0, len(assertions))
	for _, assertion := range assertions {
		object, err := modules.scopeObject(assertion.Package, assertion.Symbol)
		if err != nil {
			return nil, fmt.Errorf("assertion on %s: %w", assertion.Type, err)
		}
		name, ok := object.(*types.TypeName)
		if !ok {
			return nil, fmt.Errorf("assertion on %s: %s is a %s rather than an interface: %w",
				assertion.Type, assertion.Interface, objectKind(object), ErrAssertion)
		}
		iface, ok := name.Type().Underlying().(*types.Interface)
		if !ok {
			return nil, fmt.Errorf("assertion on %s: %s is a %s rather than an interface: %w",
				assertion.Type, assertion.Interface, objectKind(object), ErrAssertion)
		}
		index := slices.IndexFunc(exports, func(export boundExport) bool { return export.Name == assertion.Type })
		if index < 0 {
			return nil, fmt.Errorf("assertion on %s: %w", assertion.Type, ErrMissingSymbol)
		}
		subject := exports[index].object.Type()
		if assertion.Pointer {
			subject = types.NewPointer(subject)
		}
		if err := proveImplements(assertion, subject, iface); err != nil {
			return nil, err
		}
		bound = append(bound, boundAssertion{resolvedAssertion: assertion, subject: subject, iface: iface, ifaceObj: name})
	}
	return bound, nil
}

// proveImplements checks one assertion and explains the failure precisely.
//
// The Go compiler would report the same failure from the generated file, in
// terms of a declaration nobody wrote. MissingMethod distinguishes the two
// cases that need different fixes: a method the relocated type does not have at
// all, which usually means a prune removed the file that declared it, and a
// method whose signature no longer matches, which means upstream changed the
// interface or the implementation.
func proveImplements(assertion resolvedAssertion, subject types.Type, iface *types.Interface) error {
	if types.Implements(subject, iface) {
		return nil
	}
	subjectName := assertion.Type
	if assertion.Pointer {
		subjectName = "*" + subjectName
	}
	method, wrongType := types.MissingMethod(subject, iface, true)
	switch {
	case method == nil:
		return fmt.Errorf("%s does not implement %s: %w", subjectName, assertion.Interface, ErrAssertion)
	case wrongType:
		return fmt.Errorf("%s does not implement %s: method %s has the wrong signature: %w",
			subjectName, assertion.Interface, method.Name(), ErrAssertion)
	default:
		return fmt.Errorf("%s does not implement %s: method %s is missing: %w",
			subjectName, assertion.Interface, method.Name(), ErrAssertion)
	}
}

// generator renders the files of one facade.
type generator struct {
	spec    Spec
	exports []boundExport
	aliases map[*types.TypeName]string
	imports *importTable
	render  *renderer
	// declared holds every identifier the generated package declares, which no
	// import alias and no parameter name may shadow.
	declared map[string]bool
}

// newGenerator prepares a generator whose import names are not yet assigned.
func newGenerator(spec Spec, exports []boundExport, aliases map[*types.TypeName]string) *generator {
	reserved := make([]string, 0, len(exports)+1)
	reserved = append(reserved, spec.Package)
	declared := map[string]bool{spec.Package: true}
	for _, export := range exports {
		reserved = append(reserved, export.Name)
		declared[export.Name] = true
	}
	imports := newImportTable(reserved)
	return &generator{
		spec:     spec,
		exports:  exports,
		aliases:  aliases,
		imports:  imports,
		render:   &renderer{aliases: aliases, qualify: imports.qualify, internal: spec.unnameable, mode: renderSource},
		declared: declared,
	}
}

// facadeFile renders the curated public API.
//
// Declarations are rendered twice. The first pass exists only to discover which
// packages the file references, because an import's local name has to be chosen
// over the whole referenced set rather than in the order declarations happen to
// mention them. See importTable for why that matters.
func (g *generator) facadeFile() ([]byte, error) {
	if _, err := g.declarations(); err != nil {
		return nil, err
	}
	g.imports.assign()
	declarations, err := g.declarations()
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString(GeneratedHeader + "\n\n")
	// The blank line above matters: without it the marker would become the
	// package documentation comment, displacing the one doc.go carries.
	b.WriteString("package " + g.spec.Package + "\n\n")
	b.WriteString(g.imports.render())
	for _, declaration := range declarations {
		b.WriteString("\n" + declaration)
	}
	return formatSource(b.String())
}

// declarations renders every export in facade name order.
func (g *generator) declarations() ([]string, error) {
	declarations := make([]string, 0, len(g.exports))
	for _, export := range g.exports {
		declaration, err := g.declaration(export)
		if err != nil {
			return nil, fmt.Errorf("export %s (%s): %w", export.Name, export.Source, err)
		}
		declarations = append(declarations, g.doc(export)+declaration+"\n")
	}
	return declarations, nil
}

// declaration renders one export.
func (g *generator) declaration(export boundExport) (string, error) {
	switch export.Kind {
	case KindType, KindInterface:
		return g.aliasDeclaration(export)
	case KindFunc:
		return g.funcDeclaration(export)
	case KindConst:
		// A constant has no identity to preserve, so forwarding the value is
		// exact: an untyped constant stays untyped and a typed one keeps its
		// type, which is the relocated type the facade also aliases.
		return "const " + export.Name + " = " + g.qualified(export.object) + "\n", nil
	default:
		return "", fmt.Errorf("%w: kind %s cannot be rendered", ErrSpec, export.Kind)
	}
}

// aliasDeclaration renders a type or interface alias.
//
// An alias rather than a definition is the whole point. A redefinition would be
// a distinct type that merely shares a shape, so a consumer's implementation of
// a copied interface would not satisfy the copied contract and a value returned
// by the relocated code would not be assignable to the published name.
func (g *generator) aliasDeclaration(export boundExport) (string, error) {
	name, ok := export.object.(*types.TypeName)
	if !ok {
		return "", fmt.Errorf("%w: %s is not a type declaration", ErrKindMismatch, export.Source)
	}
	params, args, err := g.aliasTypeParams(name)
	if err != nil {
		return "", err
	}
	return "type " + export.Name + params + " = " + g.qualified(export.object) + args + "\n", nil
}

// aliasTypeParams renders the type parameter list of a generic alias and the
// argument list that passes it straight through.
//
// A generic type is aliased rather than refused because the alias still
// preserves identity exactly: the parameters are forwarded unchanged, so the
// published name and the relocated one denote the same instantiations. That is
// not true of a generic function, which is why one is refused and the other is
// not.
func (g *generator) aliasTypeParams(name *types.TypeName) (params, args string, err error) {
	named, ok := types.Unalias(name.Type()).(*types.Named)
	if !ok {
		return "", "", nil
	}
	list := named.Origin().TypeParams()
	if list.Len() == 0 {
		return "", "", nil
	}
	params, err = g.render.typeParams(list)
	if err != nil {
		return "", "", err
	}
	names := make([]string, list.Len())
	for i := range names {
		names[i] = list.At(i).Obj().Name()
	}
	return params, "[" + strings.Join(names, ", ") + "]", nil
}

// funcDeclaration renders a real forwarding function.
//
// It is a declaration rather than a variable holding a function value on
// purpose. A package level variable of function type is assignable by any
// consumer of the module, which would make the published behaviour of the API
// something any importer could change for every other importer in the process.
func (g *generator) funcDeclaration(export boundExport) (string, error) {
	fn, ok := export.object.(*types.Func)
	if !ok {
		return "", fmt.Errorf("%w: %s is not a function declaration", ErrKindMismatch, export.Source)
	}
	signature, ok := fn.Type().(*types.Signature)
	if !ok {
		return "", fmt.Errorf("%w: %s has no signature", ErrKindMismatch, export.Source)
	}
	params := signature.Params()
	names := g.parameterNames(params)
	rendered := make([]string, params.Len())
	arguments := make([]string, params.Len())
	for i := range rendered {
		final := signature.Variadic() && i == params.Len()-1
		typeExpr, err := g.render.variadicAware(params.At(i).Type(), final)
		if err != nil {
			return "", err
		}
		rendered[i] = names[i] + " " + typeExpr
		arguments[i] = names[i]
		if final {
			// Forwarding a variadic call without the ellipsis would pass the
			// slice as a single argument, which is a different call.
			arguments[i] += "..."
		}
	}
	results, err := g.results(signature.Results())
	if err != nil {
		return "", err
	}

	call := g.qualified(export.object) + "(" + strings.Join(arguments, ", ") + ")"
	body := "\treturn " + call + "\n"
	if signature.Results().Len() == 0 {
		body = "\t" + call + "\n"
	}
	return "func " + export.Name + "(" + strings.Join(rendered, ", ") + ")" + results + " {\n" + body + "}\n", nil
}

// results renders the result list of a forwarding declaration.
//
// Upstream result names are dropped. They document nothing a consumer can use,
// and keeping them would mean renaming any that collide with a parameter or an
// import alias, which is documentation that says the wrong thing rather than
// nothing.
func (g *generator) results(results *types.Tuple) (string, error) {
	if results.Len() == 0 {
		return "", nil
	}
	rendered := make([]string, results.Len())
	for i := range rendered {
		typeExpr, err := g.render.typ(results.At(i).Type())
		if err != nil {
			return "", err
		}
		rendered[i] = typeExpr
	}
	if len(rendered) == 1 {
		return " " + rendered[0], nil
	}
	return " (" + strings.Join(rendered, ", ") + ")", nil
}

// parameterNames chooses the parameter names of a forwarding declaration.
//
// Upstream names are kept where they can be, because they are the only
// documentation a signature carries. A name is replaced when it would capture
// something the declaration needs: an import alias used in the same signature
// or in the forwarding call, a facade name used as a type in the signature, or
// a predeclared identifier such as error that the signature may also spell as a
// type. Parameters are in scope across the whole signature, so any of those
// would resolve to the parameter and either fail to compile or, for a
// predeclared name, compile as something else.
func (g *generator) parameterNames(params *types.Tuple) []string {
	names := make([]string, params.Len())
	used := make(map[string]bool, params.Len())
	locals := g.imports.localNames()
	usable := func(name string) bool {
		return name != "" && name != "_" && token.IsIdentifier(name) &&
			token.Lookup(name) == token.IDENT &&
			types.Universe.Lookup(name) == nil &&
			!g.declared[name] && !locals[name] && !used[name]
	}
	for i := range names {
		if candidate := params.At(i).Name(); usable(candidate) {
			names[i] = candidate
			used[candidate] = true
		}
	}
	for i := range names {
		if names[i] != "" {
			continue
		}
		for suffix := i; ; suffix++ {
			candidate := "a" + strconv.Itoa(suffix)
			if usable(candidate) {
				names[i] = candidate
				used[candidate] = true
				break
			}
		}
	}
	return names
}

// qualified renders a reference to the relocated declaration an export
// forwards to, which is always package qualified because the facade declares
// the unqualified name itself.
func (g *generator) qualified(object types.Object) string {
	return g.imports.qualify(object.Pkg()) + "." + object.Name()
}

// assertionsFile renders the compile time interface assertions.
//
// The file is written even when the profile declares no assertion, so the
// published tree has the same shape either way and a consumer diffing two
// releases never sees a file appear or vanish because a profile changed.
func (g *generator) assertionsFile(assertions []boundAssertion) ([]byte, error) {
	reserved := make([]string, 0, len(g.declared))
	for name := range g.declared {
		reserved = append(reserved, name)
	}
	slices.Sort(reserved)
	imports := newImportTable(reserved)

	lines := func() []string {
		rendered := make([]string, 0, len(assertions))
		for _, assertion := range assertions {
			// A typed nil pointer asserts the pointer method set without
			// constructing anything. The value form is spelled through new
			// because that is the one spelling of a zero value that works for a
			// struct, a map, a function type, and everything else alike.
			subject := "*new(" + assertion.Type + ")"
			if assertion.Pointer {
				subject = "(*" + assertion.Type + ")(nil)"
			}
			rendered = append(rendered, "\t_ "+imports.qualify(assertion.ifaceObj.Pkg())+"."+assertion.ifaceObj.Name()+" = "+subject)
		}
		return rendered
	}
	lines()
	imports.assign()
	declarations := lines()

	var b strings.Builder
	b.WriteString(GeneratedHeader + "\n\n")
	b.WriteString("package " + g.spec.Package + "\n\n")
	b.WriteString(imports.render())
	b.WriteString("\n")
	if len(declarations) == 0 {
		b.WriteString(commentBlock("This module declares no interface assertions. The file exists so the " +
			"published tree has one shape regardless of the profile."))
		return formatSource(b.String())
	}
	local := slices.ContainsFunc(assertions, func(assertion boundAssertion) bool { return assertion.Local })
	if local {
		b.WriteString(commentBlock("These assertions prove that the published types implement the module-local compatibility interfaces selected by the profile. They establish internal consistency only and make no interoperability claim about k8s.io/apiserver."))
	} else {
		b.WriteString(commentBlock("These assertions prove that the published types implement the upstream " +
			"interfaces the relocated code was written against. The interfaces are the real ones from their " +
			"own modules rather than copies, so an assertion here is a statement about interoperability with " +
			"the wider ecosystem and not about this module's internal consistency."))
	}
	b.WriteString("var (\n" + strings.Join(declarations, "\n") + "\n)\n")
	return formatSource(b.String())
}

// doc renders the documentation comment of one generated declaration.
//
// The provenance sentence is generated rather than optional, because a reader
// who finds a name in the published API has to be able to get from it to the
// upstream declaration it forwards to without the engine. The relocated path is
// module relative: an absolute path would name the machine that ran the
// generation, which is exactly what a reproducible artefact may not contain.
func (g *generator) doc(export boundExport) string {
	var b strings.Builder
	if export.Doc != "" {
		b.WriteString(commentBlock(export.Doc))
		b.WriteString("//\n")
	}
	relocated := strings.TrimPrefix(export.Package, g.spec.ModulePath+"/")
	sentence := export.Name + " is generated by soapbox from the upstream " + export.Kind.String() +
		" " + export.Source + ", which this module relocated to " + relocated + "."
	if export.Direct {
		sentence = export.Name + " is generated by soapbox from the module-local compatibility " +
			export.Kind.String() + " " + export.Source + "."
	}
	if export.Kind == KindType || export.Kind == KindInterface {
		// One sentence rather than a paragraph. What an alias means is stated
		// once, in the package documentation; repeating the explanation on
		// every one of twenty declarations would bury the part that differs.
		sentence += " It is an alias, so the two names denote one type."
	}
	b.WriteString(commentBlock(sentence))
	return b.String()
}

// commentWidth is the column a generated comment wraps at, chosen so a comment
// and one level of indentation still fit the width Go source conventionally
// keeps.
const commentWidth = 77

// commentBlock wraps prose into line comments deterministically.
//
// The wrap is a plain greedy fill on spaces. It has to be deterministic rather
// than clever, because these bytes are committed: a wrapping that depended on
// anything but the text would show up as a diff in a release that changed
// nothing.
func commentBlock(text string) string {
	var b strings.Builder
	line := "//"
	for _, word := range strings.Fields(text) {
		if len(line)+1+len(word) > commentWidth && line != "//" {
			b.WriteString(line + "\n")
			line = "//"
		}
		line += " " + word
	}
	if line != "//" {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// formatSource runs the rendered file through go/format.
//
// Formatting here rather than trusting the renderer means the committed bytes
// are the ones gofmt produces, which is what every check in the generated
// repository compares against. A file that does not parse is reported with its
// source, because a generator bug is otherwise invisible.
func formatSource(source string) ([]byte, error) {
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w:\n%s", err, source)
	}
	return formatted, nil
}
