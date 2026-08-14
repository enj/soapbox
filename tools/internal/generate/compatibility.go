package generate

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

const compatibilityNotice = "// Soapbox local apiserver compatibility replaces upstream authorization types with module-local declarations; this mode is intentionally not API-compatible with k8s.io/apiserver.\n"

var localCompatibilityExports = []struct {
	name string
	kind string
	pkg  string
	sym  string
}{
	{name: "UserInfo", kind: "interface", pkg: "user", sym: "Info"},
	{name: "DefaultUserInfo", kind: "type", pkg: "user", sym: "DefaultInfo"},
	{name: "Attributes", kind: "interface", pkg: "authorizer", sym: "Attributes"},
	{name: "AttributesRecord", kind: "type", pkg: "authorizer", sym: "AttributesRecord"},
	{name: "Decision", kind: "type", pkg: "authorizer", sym: "Decision"},
	{name: "DecisionDeny", kind: "const", pkg: "authorizer", sym: "DecisionDeny"},
	{name: "DecisionAllow", kind: "const", pkg: "authorizer", sym: "DecisionAllow"},
	{name: "DecisionNoOpinion", kind: "const", pkg: "authorizer", sym: "DecisionNoOpinion"},
	{name: "Authorizer", kind: "interface", pkg: "authorizer", sym: "Authorizer"},
	{name: "RuleResolver", kind: "interface", pkg: "authorizer", sym: "RuleResolver"},
	{name: "ResourceRuleInfo", kind: "interface", pkg: "authorizer", sym: "ResourceRuleInfo"},
	{name: "DefaultResourceRuleInfo", kind: "type", pkg: "authorizer", sym: "DefaultResourceRuleInfo"},
	{name: "NonResourceRuleInfo", kind: "interface", pkg: "authorizer", sym: "NonResourceRuleInfo"},
	{name: "DefaultNonResourceRuleInfo", kind: "type", pkg: "authorizer", sym: "DefaultNonResourceRuleInfo"},
}

type compatibilityLayout struct {
	apiserverModule  string
	stagingDir       string
	root             string
	userImport       string
	authorizerImport string
	serviceImport    string
}

type compatibilityEdit struct {
	start  int
	end    int
	text   string
	change rewrite.Change
}

func (r *run) runCompatibility(ctx context.Context) error {
	if r.cfg.Compatibility.Apiserver == config.CompatibilityApiserverExternal {
		return nil
	}
	if r.cfg.Compatibility.Apiserver != config.CompatibilityApiserverLocal {
		return policyError(stageCompatibility, fmt.Errorf("unsupported apiserver compatibility mode %q", r.cfg.Compatibility.Apiserver))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	layout, err := r.compatibilityLayout()
	if err != nil {
		return policyError(stageCompatibility, err)
	}

	pre, err := r.applyLocalCompatibility(ctx, r.pre, layout)
	if err != nil {
		return runtimeError(stageCompatibility, fmt.Errorf("pre-prune compatibility: %w", err))
	}
	post, err := r.applyLocalCompatibility(ctx, r.post, layout)
	if err != nil {
		return runtimeError(stageCompatibility, fmt.Errorf("post-prune compatibility: %w", err))
	}
	r.pre, r.post = pre, post
	if err := installCompatibilityFiles(ctx, r.paths.PreModule, pre.Files); err != nil {
		return runtimeError(stageCompatibility, fmt.Errorf("install pre-prune compatibility: %w", err))
	}
	if err := installCompatibilityFiles(ctx, r.paths.PostModule, post.Files); err != nil {
		return runtimeError(stageCompatibility, fmt.Errorf("install post-prune compatibility: %w", err))
	}
	r.configureLocalFacade(layout)
	if !slices.Contains(r.cfg.Dependencies.ForbiddenModules, layout.apiserverModule) {
		r.cfg.Dependencies.ForbiddenModules = append(r.cfg.Dependencies.ForbiddenModules, layout.apiserverModule)
		slices.Sort(r.cfg.Dependencies.ForbiddenModules)
	}
	r.compatibilityChanges = []provenance.BehaviorChange{
		{
			Summary: "Authorization, user, decision, and rule-info types are module-local and are not assignable to k8s.io/apiserver types.",
			Cause:   "compatibility.apiserver: local",
			Detail:  "The local mode is an intentional public API break selected by the profile; external mode preserves real apiserver identity.",
		},
		{
			Summary: "ConfirmNoEscalation receives explicit user and namespace arguments instead of reading private apiserver request-context keys.",
			Cause:   "compatibility.apiserver: local",
			Detail:  "Private context key identity cannot be recreated safely outside k8s.io/apiserver, so local mode makes the inputs explicit.",
		},
	}
	return nil
}

func (r *run) compatibilityLayout() (compatibilityLayout, error) {
	var modulePath, stagingDir string
	for _, staged := range r.root.Staging {
		if path.Base(staged.Path) != "apiserver" {
			continue
		}
		if modulePath != "" {
			return compatibilityLayout{}, fmt.Errorf("source stages more than one apiserver module: %s and %s", modulePath, staged.Path)
		}
		modulePath, stagingDir = staged.Path, staged.Dir
	}
	if modulePath == "" {
		return compatibilityLayout{}, fmt.Errorf("source stages no apiserver module")
	}
	root := strings.TrimSuffix(r.cfg.Destination.InternalPrefix, "/") + "/compat/apiserver"
	importRoot := r.cfg.Destination.Module + "/" + root
	return compatibilityLayout{
		apiserverModule:  modulePath,
		stagingDir:       stagingDir,
		root:             root,
		userImport:       importRoot + "/user",
		authorizerImport: importRoot + "/authorizer",
		serviceImport:    importRoot + "/serviceaccount",
	}, nil
}

func (r *run) applyLocalCompatibility(ctx context.Context, result *extract.Result, layout compatibilityLayout) (*extract.Result, error) {
	if result == nil {
		return nil, fmt.Errorf("extraction produced no result")
	}
	files := make([]relocate.File, 0, len(result.Files.Files))
	changes := make(map[string][]rewrite.Change)
	for _, file := range result.Files.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// This adapter forwards the context-reading ConfirmNoEscalation signature
		// for the internal RBAC API. Local mode deliberately exposes only the
		// explicit-user versioned API, and the profile already prunes this adapter
		// from the published closure.
		if strings.HasSuffix(file.Source, "pkg/registry/rbac/validation/internal_version_adapter.go") {
			recordCompatibilityPruned(result, file.Source)
			continue
		}
		transformed, fileChanges, err := rewriteCompatibilityFile(file, layout)
		if err != nil {
			return nil, err
		}
		files = append(files, transformed)
		if len(fileChanges) > 0 {
			changes[file.Path] = fileChanges
		}
	}
	result.Files.Files = files
	result.Files.Packages = nil
	set, err := (relocate.FileSet{}).With(files...)
	if err != nil {
		return nil, fmt.Errorf("recompose transformed extraction: %w", err)
	}
	result.Files = set
	mergeResultChanges(result, changes)

	compatFiles, records, err := r.localCompatibilityFiles(layout)
	if err != nil {
		return nil, err
	}
	result.Provenance = append(result.Provenance, records...)
	set, err = result.Files.With(compatFiles...)
	if err != nil {
		return nil, fmt.Errorf("compose local compatibility: %w", err)
	}
	result.Files = set
	return result, nil
}

func recordCompatibilityPruned(result *extract.Result, sourcePath string) {
	for _, record := range result.Provenance {
		if sourcePath == record.SourcePackage || strings.HasPrefix(sourcePath, record.SourcePackage+"/") {
			record.AddPruned(sourcePath)
			return
		}
	}
}

func installCompatibilityFiles(ctx context.Context, root string, set relocate.FileSet) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("install compatibility module: %w", err)
	}
	// The directory is run-owned scratch. Recreate it from the transformed set so
	// a compatibility-pruned file cannot survive as stale bytes on disk.
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("reset compatibility module: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("create compatibility module: %w", err)
	}
	for _, file := range set.Files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("install compatibility module: %w", err)
		}
		destination := filepath.Join(root, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", file.Path, err)
		}
		if err := os.WriteFile(destination, file.Contents, file.Mode.FileMode().Perm()); err != nil {
			return fmt.Errorf("write %s: %w", file.Path, err)
		}
	}
	return nil
}

func rewriteCompatibilityFile(file relocate.File, layout compatibilityLayout) (relocate.File, []rewrite.Change, error) {
	if !strings.HasSuffix(file.Path, ".go") {
		return file, nil, nil
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file.Path, file.Contents, parser.ParseComments)
	if err != nil {
		return relocate.File{}, nil, fmt.Errorf("parse %s: %w", file.Path, err)
	}
	imports := map[string]string{
		layout.apiserverModule + "/pkg/authentication/user":           layout.userImport,
		layout.apiserverModule + "/pkg/authentication/serviceaccount": layout.serviceImport,
		layout.apiserverModule + "/pkg/authorization/authorizer":      layout.authorizerImport,
	}
	requestImport := layout.apiserverModule + "/pkg/endpoints/request"
	var edits []compatibilityEdit
	requestRemoved := false
	for _, spec := range parsed.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return relocate.File{}, nil, fmt.Errorf("parse import in %s: %w", file.Path, err)
		}
		if value == requestImport {
			start, end := wholeLine(file.Contents, positionOffset(fset, spec.Pos()), positionOffset(fset, spec.End()))
			edits = append(edits, compatibilityEdit{start: start, end: end, change: rewrite.Change{
				Kind: rewrite.ChangeCompatibility, Path: file.Path,
				Line: fset.Position(spec.Pos()).Line, From: string(file.Contents[start:end]),
			}})
			requestRemoved = true
			continue
		}
		to, ok := imports[value]
		if !ok {
			continue
		}
		start, end := positionOffset(fset, spec.Path.Pos()), positionOffset(fset, spec.Path.End())
		replacement := strconv.Quote(to)
		edits = append(edits, compatibilityEdit{start: start, end: end, text: replacement, change: rewrite.Change{
			Kind: rewrite.ChangeImport, Path: file.Path, Line: fset.Position(spec.Path.Pos()).Line,
			From: value, To: to,
		}})
	}
	if requestRemoved {
		confirm, err := confirmNoEscalation(parsed)
		if err != nil {
			return relocate.File{}, nil, fmt.Errorf("rewrite %s: %w", file.Path, err)
		}
		start := positionOffset(fset, confirm.Pos())
		end := positionOffset(fset, confirm.Body.Lbrace) + 1
		signature := "func ConfirmNoEscalation(ctx context.Context, ruleResolver AuthorizationRuleResolver, rules []rbacv1.PolicyRule, user user.Info, namespace string) error {"
		edits = append(edits, compatibilityEdit{start: start, end: end, text: signature, change: rewrite.Change{
			Kind: rewrite.ChangeCompatibility, Path: file.Path,
			Line: fset.Position(confirm.Pos()).Line, From: string(file.Contents[start:end]), To: signature,
		}})
		owner, err := ownerRulesStatement(confirm)
		if err != nil {
			return relocate.File{}, nil, fmt.Errorf("rewrite %s: %w", file.Path, err)
		}
		identityStart, err := identityContextStatement(confirm)
		if err != nil {
			return relocate.File{}, nil, fmt.Errorf("rewrite %s: %w", file.Path, err)
		}
		bodyStart := positionOffset(fset, identityStart.Pos())
		bodyEnd := positionOffset(fset, owner.Pos())
		edits = append(edits, compatibilityEdit{start: bodyStart, end: bodyEnd, text: "\n\t", change: rewrite.Change{
			Kind: rewrite.ChangeCompatibility, Path: file.Path,
			Line: fset.Position(confirm.Body.Lbrace).Line + 1,
			From: string(file.Contents[bodyStart:bodyEnd]), To: "explicit user and namespace arguments",
		}})
	}
	if len(edits) == 0 {
		return file, nil, nil
	}
	packageOffset := positionOffset(fset, parsed.Package)
	edits = append(edits, compatibilityEdit{start: packageOffset, end: packageOffset, text: compatibilityNotice, change: rewrite.Change{
		Kind: rewrite.ChangeNotice, Path: file.Path, Line: fset.Position(parsed.Package).Line, To: compatibilityNotice,
	}})
	contents, recorded, err := applyCompatibilityEdits(file.Contents, edits)
	if err != nil {
		return relocate.File{}, nil, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	file.Contents = contents
	return file, recorded, nil
}

func confirmNoEscalation(file *ast.File) (*ast.FuncDecl, error) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "ConfirmNoEscalation" {
			if fn.Body == nil {
				return nil, fmt.Errorf("ConfirmNoEscalation has no body")
			}
			return fn, nil
		}
	}
	return nil, fmt.Errorf("request-context import exists but ConfirmNoEscalation does not")
}

func identityContextStatement(fn *ast.FuncDecl) (ast.Stmt, error) {
	for _, stmt := range fn.Body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			continue
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if ok && ident.Name == "user" {
			return stmt, nil
		}
	}
	return nil, fmt.Errorf("ConfirmNoEscalation has no request-context user assignment")
}

func ownerRulesStatement(fn *ast.FuncDecl) (ast.Stmt, error) {
	for _, stmt := range fn.Body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			continue
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if ok && ident.Name == "ownerRules" {
			return stmt, nil
		}
	}
	return nil, fmt.Errorf("ConfirmNoEscalation has no ownerRules assignment")
}

func positionOffset(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Offset
}

func wholeLine(src []byte, start, end int) (int, int) {
	for start > 0 && src[start-1] != '\n' {
		start--
	}
	for end < len(src) && src[end] != '\n' {
		end++
	}
	if end < len(src) {
		end++
	}
	return start, end
}

func applyCompatibilityEdits(src []byte, edits []compatibilityEdit) ([]byte, []rewrite.Change, error) {
	slices.SortFunc(edits, func(a, b compatibilityEdit) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return a.end - b.end
	})
	for i := 1; i < len(edits); i++ {
		if edits[i].start < edits[i-1].end {
			return nil, nil, fmt.Errorf("overlapping edits [%d,%d) and [%d,%d)", edits[i-1].start, edits[i-1].end, edits[i].start, edits[i].end)
		}
	}
	var out strings.Builder
	out.Grow(len(src))
	cursor := 0
	changes := make([]rewrite.Change, 0, len(edits))
	for _, edit := range edits {
		if edit.start < cursor || edit.end > len(src) {
			return nil, nil, fmt.Errorf("edit [%d,%d) is outside %d bytes", edit.start, edit.end, len(src))
		}
		out.Write(src[cursor:edit.start])
		out.WriteString(edit.text)
		cursor = edit.end
		changes = append(changes, edit.change)
	}
	out.Write(src[cursor:])
	slices.SortFunc(changes, func(a, b rewrite.Change) int {
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return strings.Compare(string(a.Kind), string(b.Kind))
	})
	return []byte(out.String()), changes, nil
}

func mergeResultChanges(result *extract.Result, changes map[string][]rewrite.Change) {
	if len(changes) == 0 {
		return
	}
	updated := make(map[string]bool)
	for _, record := range result.Provenance {
		for i, file := range record.Files {
			if added := changes[file.Path]; len(added) > 0 {
				record.Files[i].Changes = append(record.Files[i].Changes, added...)
				updated[record.Package] = true
			}
		}
	}
	for _, record := range result.Provenance {
		if !updated[record.Package] {
			continue
		}
		provenancePath := path.Join(record.Package, rewrite.ProvenanceFileName)
		for i := range result.Files.Files {
			if result.Files.Files[i].Path == provenancePath {
				result.Files.Files[i].Contents = []byte(record.Render())
				break
			}
		}
	}
}

func (r *run) localCompatibilityFiles(layout compatibilityLayout) ([]relocate.File, []*rewrite.PackageProvenance, error) {
	type sourceFile struct {
		pkg        string
		name       string
		sourcePath string
		contents   string
	}
	files := []sourceFile{
		{pkg: "user", name: "user.go", sourcePath: "pkg/authentication/user/user.go", contents: localUserSource},
		{pkg: "authorizer", name: "interfaces.go", sourcePath: "pkg/authorization/authorizer/interfaces.go", contents: strings.ReplaceAll(localAuthorizerSource, "{{USER_IMPORT}}", layout.userImport)},
		{pkg: "authorizer", name: "rule.go", sourcePath: "pkg/authorization/authorizer/rule.go", contents: localRuleSource},
		{pkg: "serviceaccount", name: "util.go", sourcePath: "pkg/authentication/serviceaccount/util.go", contents: localServiceAccountSource},
	}
	byPackage := make(map[string]*rewrite.PackageProvenance)
	var output []relocate.File
	for _, source := range files {
		packagePath := layout.root + "/" + source.pkg
		upstreamPackage := layout.stagingDir + "/" + path.Dir(source.sourcePath)
		upstreamPath := layout.stagingDir + "/" + source.sourcePath
		destination := packagePath + "/" + source.name
		contents := []byte(localCompatibilityHeader(r.cfg.Source.Repository, upstreamPath, r.post.Report.Source.Commit) + source.contents)
		file := relocate.File{
			Source: upstreamPath, Path: destination,
			Package: packagePath, SourcePackage: upstreamPackage,
			Mode: relocate.ModeRegular, Contents: contents, Generated: true,
		}
		output = append(output, file)
		record := byPackage[packagePath]
		if record == nil {
			record = rewrite.NewPackageProvenance(packagePath, upstreamPackage, rewrite.Options{
				SourceRepository: r.cfg.Source.Repository,
				SourceSHA:        r.post.Report.Source.Commit,
			})
			byPackage[packagePath] = record
		}
		record.AddFile(rewrite.File{Path: destination, SourcePath: upstreamPath, Generated: true}, rewrite.Result{
			Contents: contents,
			Changes: []rewrite.Change{{
				Kind: rewrite.ChangeCompatibility, Path: destination, Line: 1,
				From: upstreamPath, To: "module-local compatibility declaration",
			}},
		})
	}
	packageNames := make([]string, 0, len(byPackage))
	for packageName := range byPackage {
		packageNames = append(packageNames, packageName)
	}
	slices.Sort(packageNames)
	records := make([]*rewrite.PackageProvenance, 0, len(packageNames))
	for _, packageName := range packageNames {
		record := byPackage[packageName]
		records = append(records, record)
		output = append(output, relocate.File{
			Path: path.Join(packageName, rewrite.ProvenanceFileName),
			Mode: relocate.ModeRegular, Contents: []byte(record.Render()),
		})
	}
	return output, records, nil
}

func (r *run) configureLocalFacade(layout compatibilityLayout) {
	imports := map[string]string{
		"user":       layout.userImport,
		"authorizer": layout.authorizerImport,
	}
	for _, export := range localCompatibilityExports {
		r.cfg.Facade.Exports = append(r.cfg.Facade.Exports, config.Export{
			Name: export.name, Kind: export.kind,
			Source: imports[export.pkg] + "." + export.sym,
			Direct: true,
		})
	}
	assertions := make([]config.InterfaceAssertion, 0, len(r.cfg.Facade.InterfaceAssertions))
	for _, assertion := range r.cfg.Facade.InterfaceAssertions {
		var symbol string
		switch {
		case strings.HasSuffix(assertion.Interface, ".Authorizer"):
			symbol = "Authorizer"
		case strings.HasSuffix(assertion.Interface, ".RuleResolver"):
			symbol = "RuleResolver"
		default:
			continue
		}
		assertions = append(assertions, config.InterfaceAssertion{
			Type: assertion.Type, Pointer: assertion.Pointer,
			Interface: layout.authorizerImport + "." + symbol, Local: true,
		})
	}
	r.cfg.Facade.InterfaceAssertions = assertions
}

func localCompatibilityHeader(repository, sourcePath, commit string) string {
	return `/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by soapbox. DO NOT EDIT.
// This module-local compatibility declaration is intentionally not type-compatible with k8s.io/apiserver.
// Upstream repository: ` + repository + `
// Upstream path: ` + sourcePath + `
// Upstream commit: ` + commit + `

`
}

const localUserSource = `package user

// Info describes an authenticated user for the module-local authorization API.
type Info interface {
	GetName() string
	GetUID() string
	GetGroups() []string
	GetExtra() map[string][]string
}

// DefaultInfo is the module-local user information record.
type DefaultInfo struct {
	Name string
	UID string
	Groups []string
	Extra map[string][]string
}

func (i *DefaultInfo) GetName() string { return i.Name }
func (i *DefaultInfo) GetUID() string { return i.UID }
func (i *DefaultInfo) GetGroups() []string { return i.Groups }
func (i *DefaultInfo) GetExtra() map[string][]string { return i.Extra }

const SystemPrivilegedGroup = "system:masters"
`

const localAuthorizerSource = `package authorizer

import (
	"context"
	"fmt"

	"{{USER_IMPORT}}"
)

// Attributes is the request surface retained RBAC evaluation reads.
type Attributes interface {
	GetUser() user.Info
	GetVerb() string
	GetNamespace() string
	GetResource() string
	GetSubresource() string
	GetName() string
	GetAPIGroup() string
	IsResourceRequest() bool
	GetPath() string
}

// AttributesRecord is a directly constructible module-local request record.
type AttributesRecord struct {
	User user.Info
	Verb string
	Namespace string
	APIGroup string
	APIVersion string
	Resource string
	Subresource string
	Name string
	ResourceRequest bool
	Path string
}

func (a AttributesRecord) GetUser() user.Info { return a.User }
func (a AttributesRecord) GetVerb() string { return a.Verb }
func (a AttributesRecord) GetNamespace() string { return a.Namespace }
func (a AttributesRecord) GetResource() string { return a.Resource }
func (a AttributesRecord) GetSubresource() string { return a.Subresource }
func (a AttributesRecord) GetName() string { return a.Name }
func (a AttributesRecord) GetAPIGroup() string { return a.APIGroup }
func (a AttributesRecord) IsResourceRequest() bool { return a.ResourceRequest }
func (a AttributesRecord) GetPath() string { return a.Path }

// Decision is a module-local authorization verdict.
type Decision int

const (
	DecisionDeny Decision = iota
	DecisionAllow
	DecisionNoOpinion
)

func (d Decision) String() string {
	switch d {
	case DecisionDeny:
		return "Deny"
	case DecisionAllow:
		return "Allow"
	case DecisionNoOpinion:
		return "NoOpinion"
	default:
		return fmt.Sprintf("Unknown (%d)", int(d))
	}
}

// Authorizer makes a module-local authorization decision.
type Authorizer interface {
	Authorize(context.Context, Attributes) (Decision, string, error)
}

// RuleResolver resolves module-local rule information for a user.
type RuleResolver interface {
	RulesFor(context.Context, user.Info, string) ([]ResourceRuleInfo, []NonResourceRuleInfo, bool, error)
}
`

const localRuleSource = `package authorizer

// ResourceRuleInfo describes allowed resource operations.
type ResourceRuleInfo interface {
	GetVerbs() []string
	GetAPIGroups() []string
	GetResources() []string
	GetResourceNames() []string
}

// DefaultResourceRuleInfo is the module-local resource rule record.
type DefaultResourceRuleInfo struct {
	Verbs []string
	APIGroups []string
	Resources []string
	ResourceNames []string
}

func (i *DefaultResourceRuleInfo) GetVerbs() []string { return i.Verbs }
func (i *DefaultResourceRuleInfo) GetAPIGroups() []string { return i.APIGroups }
func (i *DefaultResourceRuleInfo) GetResources() []string { return i.Resources }
func (i *DefaultResourceRuleInfo) GetResourceNames() []string { return i.ResourceNames }

// NonResourceRuleInfo describes allowed non-resource operations.
type NonResourceRuleInfo interface {
	GetVerbs() []string
	GetNonResourceURLs() []string
}

// DefaultNonResourceRuleInfo is the module-local non-resource rule record.
type DefaultNonResourceRuleInfo struct {
	Verbs []string
	NonResourceURLs []string
}

func (i *DefaultNonResourceRuleInfo) GetVerbs() []string { return i.Verbs }
func (i *DefaultNonResourceRuleInfo) GetNonResourceURLs() []string { return i.NonResourceURLs }
`

const localServiceAccountSource = `package serviceaccount

import "strings"

const (
	ServiceAccountUsernamePrefix = "system:serviceaccount:"
	ServiceAccountUsernameSeparator = ":"
)

// MatchesUsername compares a service-account identity without allocation.
func MatchesUsername(namespace, name, username string) bool {
	if !strings.HasPrefix(username, ServiceAccountUsernamePrefix) {
		return false
	}
	username = username[len(ServiceAccountUsernamePrefix):]
	if !strings.HasPrefix(username, namespace) {
		return false
	}
	username = username[len(namespace):]
	if !strings.HasPrefix(username, ServiceAccountUsernameSeparator) {
		return false
	}
	return username[len(ServiceAccountUsernameSeparator):] == name
}
`
