// Command gen writes the TypeScript browser API contract from the Go
// declarations in internal/api.
//
// It is run by `go generate ./internal/api` and writes both
// src/contracts/api.generated.ts (types and the endpoint table) and
// src/contracts/api.validators.generated.ts (strict runtime validators for
// every response and SSE payload type). A test in this package fails when
// either committed file is stale or generation is not deterministic.
//
// The generator reads the package with go/ast (not reflection) so field
// names, JSON tags and doc comments survive exactly. Types from
// internal/protocol are mapped to the hand-written TypeScript declarations and
// parsers in src/contracts/telemetry.ts: those wire shapes are already
// generated from the same protocol contract and are covered by
// testdata/protocol, so duplicating them here would create a second source of
// truth.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/api"
)

func main() {
	src := flag.String("src", ".", "directory containing the internal/api Go sources")
	dst := flag.String("dst", "../../src/contracts/api.generated.ts", "output TypeScript types file")
	validatorsDst := flag.String("validators-dst", "../../src/contracts/api.validators.generated.ts", "output TypeScript validators file")
	flag.Parse()
	contract, err := load(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate api types: %v\n", err)
		os.Exit(1)
	}
	typesOutput, err := generateTypes(contract)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate api types: %v\n", err)
		os.Exit(1)
	}
	validatorsOutput, err := generateValidatorsFor(contract)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate api validators: %v\n", err)
		os.Exit(1)
	}
	if err := writeFile(*dst, typesOutput); err != nil {
		fmt.Fprintf(os.Stderr, "generate api types: %v\n", err)
		os.Exit(1)
	}
	if err := writeFile(*validatorsDst, validatorsOutput); err != nil {
		fmt.Fprintf(os.Stderr, "generate api validators: %v\n", err)
		os.Exit(1)
	}
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// generate is the types-only entry point used by the drift test.
func generate(srcDir string) ([]byte, error) {
	contract, err := load(srcDir)
	if err != nil {
		return nil, err
	}
	return generateTypes(contract)
}

// generateValidators is the validators-only entry point used by the drift test.
func generateValidators(srcDir string) ([]byte, error) {
	contract, err := load(srcDir)
	if err != nil {
		return nil, err
	}
	return generateValidatorsFor(contract)
}

// packageRefs maps an imported Go type to the TypeScript declaration in
// src/contracts/telemetry.ts that already describes it.
var packageRefs = map[string]string{
	"protocol.Inventory":     "Inventory",
	"protocol.MetricsRecord": "MetricsRecord",
	"protocol.StatusMessage": "StatusMessage",
	"protocol.DetailResult":  "DetailResult",
}

// overrides replace a Go declaration with a hand-written TypeScript
// expression where the JSON shape is a union the reflection-free generator
// cannot express.
var overrides = map[string]string{
	// On the wire, a scope is the string "*" or an array of machine IDs.
	"MachineScope": `"*" | string[]`,
	// The hub stores the protocol metrics record and adds receivedAt.
	"ReceivedMetrics": `MetricsRecord & { receivedAt: string }`,
	// An SSE event id is a single string; its parts are parsed in Go.
	"LiveEventID": `string`,
	// A decoded SSE event. On the wire this is id/event/data lines, not JSON.
	"LiveFrame": `{ id: string; event: LiveEvent; data: HubSnapshot }`,
	// The webhook body is discriminated by action; the generic struct shape
	// would allow every field on every action.
	"AlertWebhookRequest": `({ action: "configure"; url: string; headers?: { [key: string]: string }; secret?: string | null; enabled?: boolean } | { action: "clear" } | { action: "test" })`,
}

// skipDirective marks an exported Go type that is not part of the JSON
// contract (an error value or a query helper) and must not be generated.
const skipDirective = "api:skip"

// responseDirective marks a type that a response or SSE payload uses directly.
// Validators are generated for these types and every local type they reach.
const responseDirective = "api:response"

// primitives maps Go built-in types to TypeScript.
var primitives = map[string]string{
	"string":  "string",
	"bool":    "boolean",
	"int":     "number",
	"int32":   "number",
	"int64":   "number",
	"uint":    "number",
	"uint32":  "number",
	"uint64":  "number",
	"float32": "number",
	"float64": "number",
	"any":     "unknown",
}

type enumInfo struct {
	values []string
}

type typeDecl struct {
	name     string
	doc      string
	spec     *ast.TypeSpec
	fields   []fieldDecl
	response bool
}

type fieldDecl struct {
	doc      string
	jsonName string
	optional bool
	typeExpr ast.Expr
	// Directive-driven bounds. Empty strings mean "use the type default".
	rangeMin  string
	rangeMax  string
	lengthMin string
	lengthMax string
	itemsMin  string
	itemsMax  string
	timestamp bool
}

type contract struct {
	decls map[string]*typeDecl
	enums map[string]*enumInfo
	names []string
}

// load parses internal/api into the generator's type model.
func load(srcDir string) (*contract, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, srcDir, func(info os.FileInfo) bool {
		name := info.Name()
		if strings.HasSuffix(name, "_test.go") {
			return false
		}
		return strings.HasSuffix(name, ".go")
	}, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("expected one package in %s, found %d", srcDir, len(pkgs))
	}
	var pkg *ast.Package
	for _, value := range pkgs {
		pkg = value
	}

	enums := make(map[string]*enumInfo)
	decls := make(map[string]*typeDecl)
	files := make([]*ast.File, 0, len(pkg.Files))
	for _, file := range pkg.Files {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		return fset.Position(files[i].Pos()).Filename < fset.Position(files[j].Pos()).Filename
	})
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gen.Tok {
			case token.TYPE:
				for _, spec := range gen.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !typeSpec.Name.IsExported() {
						continue
					}
					entry := &typeDecl{
						name:     typeSpec.Name.Name,
						doc:      docText(textOf(typeSpec.Doc, gen.Doc)),
						spec:     typeSpec,
						response: hasDirective(responseDirective, typeSpec.Doc, gen.Doc),
					}
					if hasDirective(skipDirective, typeSpec.Doc, gen.Doc) {
						continue
					}
					if structType, ok := typeSpec.Type.(*ast.StructType); ok {
						for _, field := range structType.Fields.List {
							entry.fields = append(entry.fields, parseField(field))
						}
					}
					decls[entry.name] = entry
				}
			case token.CONST:
				for _, spec := range gen.Specs {
					valueSpec, ok := spec.(*ast.ValueSpec)
					if !ok || valueSpec.Type == nil {
						continue
					}
					ident, ok := valueSpec.Type.(*ast.Ident)
					if !ok {
						continue
					}
					info := enums[ident.Name]
					if info == nil {
						info = &enumInfo{}
						enums[ident.Name] = info
					}
					for _, value := range valueSpec.Values {
						lit, ok := value.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						text, err := strconv.Unquote(lit.Value)
						if err != nil {
							return nil, fmt.Errorf("%s: %w", ident.Name, err)
						}
						info.values = append(info.values, text)
					}
				}
			}
		}
	}

	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)
	return &contract{decls: decls, enums: enums, names: names}, nil
}

// generateTypes renders api.generated.ts.
func generateTypes(c *contract) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteString("// Code generated by internal/api/gen. DO NOT EDIT.\n")
	buffer.WriteString("//\n")
	buffer.WriteString("// Run `go generate ./internal/api` to regenerate.\n")
	buffer.WriteString("// The endpoint table and the Go types in internal/api are the source of truth.\n\n")

	imports := usedTelemetryTypes(c.decls, c.names)
	if len(imports) > 0 {
		buffer.WriteString("import type { ")
		buffer.WriteString(strings.Join(imports, ", "))
		buffer.WriteString(" } from \"./telemetry\";\n\n")
	}

	for _, name := range c.names {
		decl := c.decls[name]
		if err := emitDecl(&buffer, decl, c.enums[name], c.decls); err != nil {
			return nil, err
		}
	}

	buffer.WriteString("// API_ENDPOINTS lists every route in the 1.0 browser and collector\n")
	buffer.WriteString("// HTTP contract. Recording routes are removed in 1.0 and are not listed.\n")
	buffer.WriteString("export const API_ENDPOINTS = [\n")
	for _, endpoint := range api.Endpoints() {
		fmt.Fprintf(&buffer, "  { method: %s, path: %s, auth: %s },\n",
			strconv.Quote(endpoint.Method), strconv.Quote(endpoint.Path), strconv.Quote(string(endpoint.Auth)))
	}
	buffer.WriteString("] as const;\n\n")
	buffer.WriteString("export type ApiEndpoint = (typeof API_ENDPOINTS)[number];\n")

	return buffer.Bytes(), nil
}

// usedTelemetryTypes reports which telemetry.ts declarations are referenced.
func usedTelemetryTypes(decls map[string]*typeDecl, names []string) []string {
	used := make(map[string]struct{})
	for _, name := range names {
		decl := decls[name]
		if expr, ok := overrides[name]; ok {
			for ref := range packageRefs {
				if strings.Contains(expr, packageRefs[ref]) {
					used[packageRefs[ref]] = struct{}{}
				}
			}
		}
		for _, field := range decl.fields {
			collectRefs(field.typeExpr, used)
		}
	}
	out := make([]string, 0, len(used))
	for name := range used {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func collectRefs(expr ast.Expr, used map[string]struct{}) {
	switch value := expr.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := value.X.(*ast.Ident); ok {
			if ts, ok := packageRefs[pkg.Name+"."+value.Sel.Name]; ok {
				used[ts] = struct{}{}
			}
		}
	case *ast.StarExpr:
		collectRefs(value.X, used)
	case *ast.ArrayType:
		collectRefs(value.Elt, used)
	case *ast.MapType:
		collectRefs(value.Value, used)
	case *ast.IndexExpr:
		collectRefs(value.X, used)
		collectRefs(value.Index, used)
	}
}

func emitDecl(buffer *bytes.Buffer, decl *typeDecl, enum *enumInfo, decls map[string]*typeDecl) error {
	name := decl.name
	if override, ok := overrides[name]; ok {
		writeDoc(buffer, decl.doc)
		fmt.Fprintf(buffer, "export type %s = %s;\n\n", name, override)
		return nil
	}
	if _, ok := decl.spec.Type.(*ast.StructType); ok {
		writeDoc(buffer, decl.doc)
		fmt.Fprintf(buffer, "export interface %s {\n", name)
		for _, field := range decl.fields {
			if err := emitField(buffer, field); err != nil {
				return fmt.Errorf("%s.%s: %w", name, field.jsonName, err)
			}
		}
		buffer.WriteString("}\n\n")
		return nil
	}
	// A named non-struct type: emit an alias, using the enum values when the
	// declaration is a string type with constants.
	writeDoc(buffer, decl.doc)
	if enum != nil && len(enum.values) > 0 {
		parts := make([]string, len(enum.values))
		for i, value := range enum.values {
			parts[i] = strconv.Quote(value)
		}
		fmt.Fprintf(buffer, "export type %s = %s;\n\n", name, strings.Join(parts, " | "))
		return nil
	}
	expr, err := tsType(decl.spec.Type)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Fprintf(buffer, "export type %s = %s;\n\n", name, expr)
	return nil
}

func emitField(buffer *bytes.Buffer, field fieldDecl) error {
	if field.jsonName == "" {
		return fmt.Errorf("field has no JSON name")
	}
	optional := field.optional
	nullable := false
	expr := field.typeExpr
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
		if isOptionalType(expr) {
			nullable = true
		} else if !optional {
			nullable = true
		}
	}
	text, err := tsType(expr)
	if err != nil {
		return err
	}
	if nullable {
		text = nullableType(text)
	}
	writeDoc(buffer, field.doc)
	if optional {
		fmt.Fprintf(buffer, "  %s?: %s;\n", field.jsonName, text)
	} else {
		fmt.Fprintf(buffer, "  %s: %s;\n", field.jsonName, text)
	}
	return nil
}

// isOptionalType reports whether expr is protocol.Optional[T].
func isOptionalType(expr ast.Expr) bool {
	index, ok := expr.(*ast.IndexExpr)
	if !ok {
		return false
	}
	selector, ok := index.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "protocol" && selector.Sel.Name == "Optional"
}

func nullableType(text string) string {
	if strings.Contains(text, "|") {
		return "(" + text + ") | null"
	}
	return text + " | null"
}

// tsType renders a Go type expression as TypeScript.
func tsType(expr ast.Expr) (string, error) {
	switch value := expr.(type) {
	case *ast.Ident:
		if primitive, ok := primitives[value.Name]; ok {
			return primitive, nil
		}
		return value.Name, nil
	case *ast.SelectorExpr:
		pkg, ok := value.X.(*ast.Ident)
		if !ok {
			return "", fmt.Errorf("unsupported selector expression")
		}
		if ts, ok := packageRefs[pkg.Name+"."+value.Sel.Name]; ok {
			return ts, nil
		}
		return "", fmt.Errorf("unsupported imported type %s.%s", pkg.Name, value.Sel.Name)
	case *ast.StarExpr:
		inner, err := tsType(value.X)
		if err != nil {
			return "", err
		}
		return nullableType(inner), nil
	case *ast.ArrayType:
		inner, err := tsType(value.Elt)
		if err != nil {
			return "", err
		}
		if strings.Contains(inner, "|") || strings.Contains(inner, "=>") {
			return "(" + inner + ")[]", nil
		}
		return inner + "[]", nil
	case *ast.MapType:
		key, err := tsType(value.Key)
		if err != nil {
			return "", err
		}
		inner, err := tsType(value.Value)
		if err != nil {
			return "", err
		}
		return "{ [key: " + key + "]: " + inner + " }", nil
	case *ast.IndexExpr:
		// protocol.Optional[T] is represented as T | null at the field level.
		if isOptionalType(expr) {
			return tsType(value.Index)
		}
		return "", fmt.Errorf("unsupported generic type")
	case *ast.InterfaceType:
		return "unknown", nil
	default:
		return "", fmt.Errorf("unsupported Go type %T", expr)
	}
}

func parseField(field *ast.Field) fieldDecl {
	out := fieldDecl{doc: docText(textOf(field.Doc, field.Comment))}
	if len(field.Names) == 1 {
		out.jsonName = field.Names[0].Name
	}
	out.typeExpr = field.Type
	directives := fieldDirectives(field)
	if raw, ok := directives["range"]; ok {
		out.rangeMin, out.rangeMax = splitRange(raw)
	}
	if raw, ok := directives["length"]; ok {
		out.lengthMin, out.lengthMax = splitRange(raw)
	}
	if raw, ok := directives["items"]; ok {
		out.itemsMin, out.itemsMax = splitRange(raw)
	}
	if _, ok := directives["timestamp"]; ok {
		out.timestamp = true
	}
	if field.Tag == nil {
		return out
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return out
	}
	for _, part := range strings.Split(tag, " ") {
		if !strings.HasPrefix(part, `json:"`) {
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(part, `json:"`), `"`)
		pieces := strings.Split(body, ",")
		if pieces[0] != "" && pieces[0] != "-" {
			out.jsonName = pieces[0]
		}
		for _, option := range pieces[1:] {
			if option == "omitempty" {
				out.optional = true
			}
		}
	}
	return out
}

func fieldDirectives(field *ast.Field) map[string]string {
	out := make(map[string]string)
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(comment.Text)
			if !strings.HasPrefix(text, "//api:") {
				continue
			}
			body := strings.TrimPrefix(text, "//api:")
			key, value, _ := strings.Cut(body, "=")
			out[key] = value
		}
	}
	return out
}

func splitRange(raw string) (string, string) {
	left, right, found := strings.Cut(raw, "..")
	if !found {
		return "", ""
	}
	return strings.TrimSpace(left), strings.TrimSpace(right)
}

func writeDoc(buffer *bytes.Buffer, text string) {
	if text == "" {
		return
	}
	buffer.WriteString("/**\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.ReplaceAll(line, "*/", "* /")
		if line == "" {
			buffer.WriteString(" *\n")
			continue
		}
		buffer.WriteString(" * " + line + "\n")
	}
	buffer.WriteString(" */\n")
}

// hasDirective reports whether any group carries "//<directive>". Doc text
// omits comment directives, so the raw comments are inspected.
func hasDirective(directive string, groups ...*ast.CommentGroup) bool {
	wanted := "//" + directive
	for _, group := range groups {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			if strings.TrimSpace(comment.Text) == wanted {
				return true
			}
		}
	}
	return false
}

func textOf(groups ...*ast.CommentGroup) string {
	for _, group := range groups {
		if group == nil || len(group.List) == 0 {
			continue
		}
		return docText(group.Text())
	}
	return ""
}

// docText trims and normalizes a Go doc comment for JSDoc and removes
// generator directives.
func docText(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "api:") {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ---------------------------------------------------------------------------
// Validator generation
// ---------------------------------------------------------------------------

// generateValidatorsFor renders api.validators.generated.ts.
func generateValidatorsFor(c *contract) ([]byte, error) {
	responseNames, err := responseClosure(c)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.WriteString("// Code generated by internal/api/gen. DO NOT EDIT.\n")
	buffer.WriteString("//\n")
	buffer.WriteString("// Run `go generate ./internal/api` to regenerate.\n")
	buffer.WriteString("// Strict runtime validators for every response and SSE payload type.\n\n")
	buffer.WriteString("import type * as Api from \"./api.generated\";\n")
	buffer.WriteString("import type { DetailResult, StatusMessage } from \"./telemetry\";\n")
	buffer.WriteString("import { parseCollectorMessage, parseInventory, parseTelemetryRecord } from \"./telemetry\";\n\n")
	writeValidatorPrelude(&buffer)
	for _, name := range responseNames {
		decl := c.decls[name]
		if err := emitValidator(&buffer, decl, c); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	buffer.WriteString("// API_RESPONSE_VALIDATORS maps a contract type name to its validator.\n")
	buffer.WriteString("export const API_RESPONSE_VALIDATORS = {\n")
	for _, name := range responseNames {
		fmt.Fprintf(&buffer, "  %s: validate%s,\n", name, name)
	}
	buffer.WriteString("} as const;\n")
	return buffer.Bytes(), nil
}

// responseClosure returns the sorted response/SSE types and every local type
// they reach through struct fields, slices and maps.
func responseClosure(c *contract) ([]string, error) {
	seen := make(map[string]bool)
	var visit func(name string) error
	visit = func(name string) error {
		if seen[name] {
			return nil
		}
		decl := c.decls[name]
		if decl == nil {
			return fmt.Errorf("unknown referenced type %q", name)
		}
		seen[name] = true
		for _, field := range decl.fields {
			if err := visitExpr(c, field.typeExpr, visit); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range c.names {
		if c.decls[name].response {
			if err := visit(name); err != nil {
				return nil, err
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func visitExpr(c *contract, expr ast.Expr, visit func(string) error) error {
	switch value := expr.(type) {
	case *ast.Ident:
		if _, primitive := primitives[value.Name]; primitive {
			return nil
		}
		if c.decls[value.Name] != nil {
			return visit(value.Name)
		}
		return nil
	case *ast.SelectorExpr:
		return nil
	case *ast.StarExpr:
		return visitExpr(c, value.X, visit)
	case *ast.ArrayType:
		return visitExpr(c, value.Elt, visit)
	case *ast.MapType:
		return visitExpr(c, value.Value, visit)
	case *ast.IndexExpr:
		if isOptionalType(expr) {
			return visitExpr(c, value.Index, visit)
		}
		return visitExpr(c, value.X, visit)
	default:
		return nil
	}
}

func writeValidatorPrelude(buffer *bytes.Buffer) {
	buffer.WriteString(`type Obj = Record<string, unknown>;

function __object(value: unknown, what: string): Obj {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("invalid " + what + ": expected object, got " + __describe(value));
  }
  return value as Obj;
}

function __keys(o: Obj, required: readonly string[], optional: readonly string[], what: string): void {
  const allowed = new Set([...required, ...optional]);
  for (const key of Object.keys(o)) {
    if (!allowed.has(key)) throw new Error("invalid " + what + ": unexpected field \"" + key + "\"");
  }
  for (const key of required) {
    if (!Object.prototype.hasOwnProperty.call(o, key) || o[key] === undefined) {
      throw new Error("invalid " + what + ": missing field \"" + key + "\"");
    }
  }
  for (const key of optional) {
    if (Object.prototype.hasOwnProperty.call(o, key) && o[key] === undefined) {
      throw new Error("invalid " + what + ": " + key + " must not be undefined");
    }
  }
}

function __array(value: unknown, what: string, min: number, max: number): unknown[] {
  if (!Array.isArray(value)) throw new Error("invalid " + what + ": expected array, got " + __describe(value));
  if (value.length < min || value.length > max) {
    throw new Error("invalid " + what + ": length " + value.length + " outside " + min + ".." + max);
  }
  return value;
}

function __map(value: unknown, what: string): Obj {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("invalid " + what + ": expected object, got " + __describe(value));
  }
  return value as Obj;
}

function __string(value: unknown, what: string, min = 0, max = Number.MAX_SAFE_INTEGER): string {
  if (typeof value !== "string") throw new Error("invalid " + what + ": expected string, got " + __describe(value));
  if (value.length < min || value.length > max) {
    throw new Error("invalid " + what + ": length " + value.length + " outside " + min + ".." + max);
  }
  return value;
}

function __id(value: unknown, what: string, min = 1, max = 128): string {
  const text = __string(value, what, min, max);
  for (let index = 0; index < text.length; index += 1) {
    const code = text.charCodeAt(index);
    if (code < 0x21 || code > 0x7e) throw new Error("invalid " + what + ": must be printable ASCII without spaces");
  }
  return text;
}

function __bool(value: unknown, what: string): boolean {
  if (typeof value !== "boolean") throw new Error("invalid " + what + ": expected boolean, got " + __describe(value));
  return value;
}

function __int(value: unknown, what: string, min = Number.MIN_SAFE_INTEGER, max = Number.MAX_SAFE_INTEGER): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) {
    throw new Error("invalid " + what + ": expected safe integer, got " + __describe(value));
  }
  if (value < min || value > max) throw new Error("invalid " + what + ": " + value + " outside " + min + ".." + max);
  return value;
}

function __number(value: unknown, what: string, min = -Number.MAX_VALUE, max = Number.MAX_VALUE): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error("invalid " + what + ": expected finite number, got " + __describe(value));
  }
  if (value < min || value > max) throw new Error("invalid " + what + ": " + value + " outside " + min + ".." + max);
  return value;
}

function __enum<T extends string>(value: unknown, allowed: readonly T[], what: string): T {
  if (typeof value !== "string" || !(allowed as readonly string[]).includes(value)) {
    throw new Error("invalid " + what + ": unexpected value " + __describe(value));
  }
  return value as T;
}

function __timestamp(value: unknown, what: string): string {
  const text = __string(value, what, 1, 64);
  const epoch = Date.parse(text);
  if (!Number.isFinite(epoch) || epoch < Date.parse("2000-01-01T00:00:00Z") || epoch > Date.parse("2200-01-01T00:00:00Z")) {
    throw new Error("invalid " + what + ": not a parseable timestamp");
  }
  return text;
}

function __nullable<T>(value: unknown, parse: (v: unknown) => T, what: string): T | null {
  if (value === null) return null;
  if (value === undefined) throw new Error("invalid " + what + ": must not be undefined");
  return parse(value);
}

function __optional<T>(value: unknown, parse: (v: unknown) => T, what: string): T | null {
  if (value === undefined) throw new Error("invalid " + what + ": must not be undefined");
  if (value === null) return null;
  return parse(value);
}

function __value<T>(value: unknown, parse: (v: unknown) => T, what: string): T {
  if (value === undefined) throw new Error("invalid " + what + ": must not be undefined");
  if (value === null) throw new Error("invalid " + what + ": must not be null");
  return parse(value);
}

function __collectorMessage<T extends { type: string }>(value: unknown, what: string, type: T["type"]): T {
  const parsed = parseCollectorMessage(value) as unknown as T;
  if (parsed.type !== type) throw new Error("invalid " + what + ": expected " + type + ", got " + __describe(parsed.type));
  return parsed;
}

function __describe(value: unknown): string {
  try {
    const text = JSON.stringify(value);
    if (typeof text === "string") return text.length <= 80 ? text : text.slice(0, 77) + "...";
  } catch {
    // fall through
  }
  return String(value);
}

`)
}

func emitValidator(buffer *bytes.Buffer, decl *typeDecl, c *contract) error {
	name := decl.name
	switch name {
	case "MachineScope":
		buffer.WriteString(`export function validateMachineScope(value: unknown, what = "MachineScope"): Api.MachineScope {
  if (value === "*") return "*";
  const items = __array(value, what, 0, Number.MAX_SAFE_INTEGER);
  const seen = new Set<string>();
  const ids = items.map((item, index) => {
    const id = __id(item, what + "[" + index + "]", 1, 128);
    if (seen.has(id)) throw new Error("invalid " + what + ": duplicate machine ID " + id);
    seen.add(id);
    return id;
  });
  return ids;
}

`)
		return nil
	case "ReceivedMetrics":
		buffer.WriteString(`export function validateReceivedMetrics(value: unknown, what = "ReceivedMetrics"): Api.ReceivedMetrics {
  const o = __object(value, what);
  __keys(o, ["collectorId", "runId", "bootId", "sequence", "capturedAt", "kind", "payload", "receivedAt"], [], what);
  const { receivedAt, ...record } = o;
  const parsed = parseTelemetryRecord(record, what);
  if (parsed.kind !== "metrics") throw new Error("invalid " + what + ": expected a metrics record");
  return { ...parsed, receivedAt: __timestamp(receivedAt, what + ".receivedAt") };
}

`)
		return nil
	case "MachineState":
		buffer.WriteString(`export function validateMachineState(value: unknown, what = "MachineState"): Api.MachineState {
  const o = __object(value, what);
  __keys(o, ["machineId", "collectorId", "hostname", "connected", "lastReceivedAt"], ["expectedIntervalMs", "metrics", "inventory", "delivery", "displayName", "tags"], what);
  const machineId = __id(o.machineId, what + ".machineId", 1, 128);
  const collectorId = __id(o.collectorId, what + ".collectorId", 1, 128);
  const hostname = __string(o.hostname, what + ".hostname", 0, 255);
  const connected = __bool(o.connected, what + ".connected");
  const lastReceivedAt = __nullable(o.lastReceivedAt, (v) => __timestamp(v, what + ".lastReceivedAt"), what + ".lastReceivedAt");
  const result: Api.MachineState = { machineId, collectorId, hostname, connected, lastReceivedAt };
  if (o.expectedIntervalMs !== undefined) {
    result.expectedIntervalMs = __optional(o.expectedIntervalMs, (v) => __int(v, what + ".expectedIntervalMs", 100, 7_200_000), what + ".expectedIntervalMs");
  }
  if (o.metrics !== undefined) {
    const metrics = validateReceivedMetrics(o.metrics, what + ".metrics");
    if (metrics.collectorId !== collectorId) throw new Error("invalid " + what + ": conflicting machine metrics");
    if (lastReceivedAt === null || metrics.receivedAt !== lastReceivedAt) {
      throw new Error("invalid " + what + ": conflicting metrics receipt time");
    }
    result.metrics = metrics;
  }
  if (o.inventory !== undefined) result.inventory = parseInventory(o.inventory);
  if (o.delivery !== undefined) {
    result.delivery = __collectorMessage<StatusMessage>(o.delivery, what + ".delivery", "status");
  }
  if (o.displayName !== undefined) {
    result.displayName = __optional(o.displayName, (v) => __string(v, what + ".displayName", 0, 128), what + ".displayName");
  }
  if (o.tags !== undefined) {
    const tags = __array(o.tags, what + ".tags", 0, 32);
    const seen = new Set<string>();
    result.tags = tags.map((tag, index) => {
      const text = __string(tag, what + ".tags[" + index + "]", 1, 64);
      if (text.trim() !== text) throw new Error("invalid " + what + ": tags must be trimmed");
      if (seen.has(text)) throw new Error("invalid " + what + ": duplicate tag " + text);
      seen.add(text);
      return text;
    });
  }
  return result;
}

`)
		return nil
	case "HubSnapshot":
		buffer.WriteString(`export function validateHubSnapshot(value: unknown, what = "HubSnapshot"): Api.HubSnapshot {
  const o = __object(value, what);
  __keys(o, ["hubRunId", "machines"], [], what);
  const hubRunId = __id(o.hubRunId, what + ".hubRunId", 1, 128);
  const items = __array(o.machines, what + ".machines", 0, Number.MAX_SAFE_INTEGER);
  const seen = new Set<string>();
  const machines = items.map((machine, index) => {
    const parsed = validateMachineState(machine, what + ".machines[" + index + "]");
    if (seen.has(parsed.machineId)) throw new Error("invalid " + what + ": duplicate snapshot machine " + parsed.machineId);
    seen.add(parsed.machineId);
    return parsed;
  });
  return { hubRunId, machines };
}

`)
		return nil
	}
	enum := c.enums[name]
	if enum != nil && len(enum.values) > 0 {
		parts := make([]string, len(enum.values))
		for i, value := range enum.values {
			parts[i] = strconv.Quote(value)
		}
		fmt.Fprintf(buffer, "export function validate%s(value: unknown, what = %q): Api.%s {\n", name, name, name)
		fmt.Fprintf(buffer, "  return __enum(value, [%s], what);\n", strings.Join(parts, ", "))
		buffer.WriteString("}\n\n")
		return nil
	}
	if _, ok := decl.spec.Type.(*ast.StructType); ok {
		if err := emitStructValidator(buffer, decl, c); err != nil {
			return err
		}
		return nil
	}
	expr, err := validatorTypeExpr(decl.spec.Type, c, "value", "what", fieldDecl{})
	if err != nil {
		return err
	}
	fmt.Fprintf(buffer, "export function validate%s(value: unknown, what = %q): Api.%s {\n", name, name, name)
	if _, ok := decl.spec.Type.(*ast.MapType); ok {
		fmt.Fprintf(buffer, "  return %s as Api.%s;\n", expr, name)
	} else {
		fmt.Fprintf(buffer, "  return %s;\n", expr)
	}
	buffer.WriteString("}\n\n")
	return nil
}

func emitStructValidator(buffer *bytes.Buffer, decl *typeDecl, c *contract) error {
	name := decl.name
	var required, optional []string
	for _, field := range decl.fields {
		if field.jsonName == "" {
			continue
		}
		if field.optional {
			optional = append(optional, strconv.Quote(field.jsonName))
		} else {
			required = append(required, strconv.Quote(field.jsonName))
		}
	}
	fmt.Fprintf(buffer, "export function validate%s(value: unknown, what = %q): Api.%s {\n", name, name, name)
	buffer.WriteString("  const o = __object(value, what);\n")
	fmt.Fprintf(buffer, "  __keys(o, [%s], [%s], what);\n", strings.Join(required, ", "), strings.Join(optional, ", "))
	buffer.WriteString("  return {\n")
	for _, field := range decl.fields {
		if field.jsonName == "" {
			continue
		}
		line, err := validatorFieldLine(field, c)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", name, field.jsonName, err)
		}
		buffer.WriteString(line)
	}
	buffer.WriteString("  };\n")
	buffer.WriteString("}\n\n")
	return nil
}

func validatorFieldLine(field fieldDecl, c *contract) (string, error) {
	path := "what + " + strconv.Quote("."+field.jsonName)
	value := "o." + field.jsonName
	expr := field.typeExpr
	optionalNullable := false
	isPointer := false
	if star, ok := expr.(*ast.StarExpr); ok {
		isPointer = true
		if isOptionalType(star.X) {
			optionalNullable = true
			// protocol.Optional[T]: validate T, null allowed.
			inner, err := validatorTypeExpr(star.X.(*ast.IndexExpr).Index, c, "v", path, field)
			if err != nil {
				return "", err
			}
			if field.optional {
				return fmt.Sprintf("    ...(%s === undefined ? {} : { %s: __optional(%s, (v) => %s, %s) }),\n",
					value, field.jsonName, value, inner, path), nil
			}
			return fmt.Sprintf("    %s: __optional(%s, (v) => %s, %s),\n", field.jsonName, value, inner, path), nil
		}
		expr = star.X
	}
	if isPointer && !optionalNullable {
		inner, err := validatorTypeExpr(expr, c, "v", path, field)
		if err != nil {
			return "", err
		}
		// A required pointer is nullable; an optional pointer is not.
		if field.optional {
			return fmt.Sprintf("    ...(%s === undefined ? {} : { %s: __value(%s, (v) => %s, %s) }),\n",
				value, field.jsonName, value, inner, path), nil
		}
		return fmt.Sprintf("    %s: __nullable(%s, (v) => %s, %s),\n", field.jsonName, value, inner, path), nil
	}
	valueExpr := "o." + field.jsonName
	inner, err := validatorTypeExpr(expr, c, valueExpr, path, field)
	if err != nil {
		return "", err
	}
	if field.optional {
		return fmt.Sprintf("    ...(%s === undefined ? {} : { %s: %s }),\n", value, field.jsonName, inner), nil
	}
	return fmt.Sprintf("    %s: %s,\n", field.jsonName, inner), nil
}

// validatorTypeExpr renders validation code for one Go type expression. value
// and path are JavaScript expressions.
func validatorTypeExpr(expr ast.Expr, c *contract, value, path string, field fieldDecl) (string, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		if t.Name == "string" {
			if field.timestamp || isTimestampName(field.jsonName) {
				return fmt.Sprintf("__timestamp(%s, %s)", value, path), nil
			}
			min, max := bounds(field.lengthMin, field.lengthMax, "0", "Number.MAX_SAFE_INTEGER")
			return fmt.Sprintf("__string(%s, %s, %s, %s)", value, path, min, max), nil
		}
		if t.Name == "bool" {
			return fmt.Sprintf("__bool(%s, %s)", value, path), nil
		}
		if isIntegerName(t.Name) {
			min, max := bounds(field.rangeMin, field.rangeMax, "Number.MIN_SAFE_INTEGER", "Number.MAX_SAFE_INTEGER")
			return fmt.Sprintf("__int(%s, %s, %s, %s)", value, path, min, max), nil
		}
		if isFloatName(t.Name) {
			min, max := bounds(field.rangeMin, field.rangeMax, "-Number.MAX_VALUE", "Number.MAX_VALUE")
			return fmt.Sprintf("__number(%s, %s, %s, %s)", value, path, min, max), nil
		}
		if t.Name == "any" {
			return value, nil
		}
		if enum := c.enums[t.Name]; enum != nil && len(enum.values) > 0 {
			parts := make([]string, len(enum.values))
			for i, v := range enum.values {
				parts[i] = strconv.Quote(v)
			}
			return fmt.Sprintf("__enum(%s, [%s], %s)", value, strings.Join(parts, ", "), path), nil
		}
		if c.decls[t.Name] != nil {
			return fmt.Sprintf("validate%s(%s, %s)", t.Name, value, path), nil
		}
		return "", fmt.Errorf("unsupported identifier %q", t.Name)
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return "", fmt.Errorf("unsupported selector expression")
		}
		switch pkg.Name + "." + t.Sel.Name {
		case "protocol.Inventory":
			return fmt.Sprintf("parseInventory(%s)", value), nil
		case "protocol.StatusMessage":
			return fmt.Sprintf("__collectorMessage<StatusMessage>(%s, %s, %q)", value, path, "status"), nil
		case "protocol.DetailResult":
			return fmt.Sprintf("__collectorMessage<DetailResult>(%s, %s, %q)", value, path, "detail.result"), nil
		case "protocol.MetricsRecord":
			return fmt.Sprintf("parseTelemetryRecord(%s, %s)", value, path), nil
		default:
			return "", fmt.Errorf("unsupported imported type %s.%s", pkg.Name, t.Sel.Name)
		}
	case *ast.StarExpr:
		inner, err := validatorTypeExpr(t.X, c, "v", path, field)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("__nullable(%s, (v) => %s, %s)", value, inner, path), nil
	case *ast.ArrayType:
		elemPath := "(" + path + " + \"[\" + index + \"]\")"
		inner, err := validatorTypeExpr(t.Elt, c, "item", elemPath, fieldDecl{})
		if err != nil {
			return "", err
		}
		min, max := bounds(field.itemsMin, field.itemsMax, "0", "Number.MAX_SAFE_INTEGER")
		return fmt.Sprintf("__array(%s, %s, %s, %s).map((item, index) => %s)", value, path, min, max, inner), nil
	case *ast.MapType:
		elemPath := "(" + path + " + \".\" + key)"
		inner, err := validatorTypeExpr(t.Value, c, "item", elemPath, fieldDecl{})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(() => { const m = __map(%s, %s); const out: Record<string, unknown> = {}; for (const [key, item] of Object.entries(m)) out[key] = %s; return out; })()",
			value, path, inner), nil
	case *ast.IndexExpr:
		if isOptionalType(expr) {
			return validatorTypeExpr(t.Index, c, value, path, field)
		}
		return "", fmt.Errorf("unsupported generic type")
	case *ast.InterfaceType:
		return value, nil
	default:
		return "", fmt.Errorf("unsupported Go type %T", expr)
	}
}

func isIntegerName(name string) bool {
	switch name {
	case "int", "int32", "int64", "uint", "uint32", "uint64":
		return true
	default:
		return false
	}
}

func isFloatName(name string) bool {
	return name == "float32" || name == "float64"
}

func isTimestampName(name string) bool {
	return name == "at" || strings.HasSuffix(name, "At")
}

func bounds(min, max, fallbackMin, fallbackMax string) (string, string) {
	if min == "" {
		min = fallbackMin
	}
	if max == "" {
		max = fallbackMax
	}
	return min, max
}
