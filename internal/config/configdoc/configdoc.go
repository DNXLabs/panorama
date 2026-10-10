// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

// Package configdoc derives the configuration reference from the source that
// implements it.
//
// WHY THIS IS DERIVED AND NOT WRITTEN. The reference is a table of every key,
// where to set it, and what it defaults to - four facts per key across forty
// keys, each of which lives in a different place in config.go: the yaml tag on
// the field, the environment variable in applyEnv, the literal in Default(),
// and the doc comment above the field. A hand-written table is four
// opportunities per key to be wrong, and wrong in the way documentation is
// worst: confidently, about a detail nobody re-reads.
//
// So the four are read from where they already are. Adding a key to config.go
// adds a row; renaming one renames the row; changing a default changes the
// figure. Nothing is remembered.
//
// The three sources and why each is read the way it is:
//
//   - THE TREE AND THE COMMENTS come from go/ast, because reflection cannot
//     see a doc comment and the prose above each field is the only description
//     of it anybody has written.
//   - THE ENVIRONMENT VARIABLES come from go/ast over applyEnv, because they
//     are string literals paired with a field pointer - `str("PANORAMA_X",
//     &cfg.Server.BaseURL)` - and that pairing is the only statement anywhere
//     of which variable sets which field. Deriving them from the field path
//     instead would be a second rule that agrees with applyEnv until somebody
//     spells one differently.
//   - THE DEFAULTS come from reflection over Default(), because they are Go
//     expressions - `32 << 20`, `30 * 24 * time.Hour` - and rendering what
//     they evaluate to is the whole point. Reading the literal would put
//     "32 << 20" in a table meant to say "32 MiB".
package configdoc

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Key is one configuration key, as the reference renders it.
type Key struct {
	// Path is the dotted location in the YAML file, e.g. "server.base_url".
	Path string `json:"path"`
	// Name is the Go field name. It is carried so the renderer can strip the
	// "Addr is ..." that Go doc convention puts at the front of every comment:
	// beside a column that already holds the key, repeating it reads as a
	// stutter, and stripping it needs the exact spelling ("DSN", "SSLMode")
	// rather than one guessed back from the yaml tag.
	Name string `json:"name"`
	// Env is the environment variable that sets it, empty when only the file
	// can. The environment wins over the file.
	Env string `json:"env,omitempty"`
	// Type is the Go type, rendered for a reader: "duration", "bytes", "list".
	Type string `json:"type"`
	// Default is the value a deployment that sets nothing gets.
	Default string `json:"default"`
	// Doc is the field's comment, whole. The renderer decides how much to use.
	Doc string `json:"doc,omitempty"`
	// Section is the top-level block it sits under, for grouping.
	Section string `json:"section"`
}

// Collect reads the config package at dir and returns every key, ordered by
// the file's own order rather than alphabetically: the file is grouped by
// concern, and a reader scanning for "the OIDC ones" wants them together.
func Collect(dir string, defaults any) ([]Key, error) {
	fset := gotoken.NewFileSet()

	// One file at a time rather than parser.ParseDir, which is deprecated: it
	// ignores build tags, and it hands back a map of packages whose order is
	// random. os.ReadDir sorts by name, so the files - and with them anything
	// read off them in order - come back the same way on every run.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	structs := map[string]*ast.StructType{}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", filepath.Join(dir, name), err)
		}
		files = append(files, f)
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != gotoken.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[ts.Name.Name] = st
				}
			}
		}
	}

	if _, ok := structs["Config"]; !ok {
		return nil, fmt.Errorf("no Config struct in %s: this generator can no longer see the configuration", dir)
	}

	env := envVars(files)
	for path, name := range customEnv {
		env[path] = name
	}
	if err := allVariablesAccountedFor(files, env); err != nil {
		return nil, err
	}

	var keys []Key
	walk(structs, "Config", "", "", "", env, reflect.ValueOf(defaults), &keys)
	return keys, nil
}

// customEnv is the handful of variables applyEnv reads by hand rather than
// through a two-argument helper, because each needs bespoke parsing - a map
// written as `model=tokens,model=tokens`, a boolean with its own error.
//
// It is a hand-maintained list, which is exactly what the rest of this package
// exists to avoid, so it is guarded rather than trusted:
// allVariablesAccountedFor refuses to emit a reference while any PANORAMA_
// variable in the package is attributed to neither a field nor this map. A new
// hand-parsed key therefore fails the build instead of quietly appearing in
// the table as "file only", which is the one wrong answer the table can give.
var customEnv = map[string]string{
	"Bedrock.ContextWindows": "PANORAMA_BEDROCK_CONTEXT_WINDOWS",
	"Pricing.Models":         "PANORAMA_PRICING_MODELS",
	"Attribution.Enabled":    "PANORAMA_ATTRIBUTION_ENABLED",
}

// envName is the shape of a variable name and nothing else. The package is
// full of strings that begin "PANORAMA_" and are not names - every error
// message about a variable starts with the variable it is about, followed by a
// colon and a format verb - and treating one of those as an unattributed key
// would fail the build over a sentence.
var envName = regexp.MustCompile(`^PANORAMA_[A-Z0-9_]+$`)

// allVariablesAccountedFor is the guard described on customEnv.
func allVariablesAccountedFor(files []*ast.File, env map[string]string) error {
	attributed := map[string]bool{}
	for _, name := range env {
		attributed[name] = true
	}

	var unknown []string
	seen := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil || !envName.MatchString(name) {
				return true
			}
			// An error message mentions the variable it is about, so names are
			// checked as a set rather than per occurrence.
			if attributed[name] || seen[name] {
				return true
			}
			seen[name] = true
			unknown = append(unknown, name)
			return true
		})
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf(
			"these variables are read by the config package but reach no documented field:\n  %s\n\n"+
				"If one sets a key, add its field path to customEnv in configdoc.go so the reference\n"+
				"shows it. Leaving it unlisted would publish a table that says the key is file-only, which is worse than\n"+
				"saying nothing",
			strings.Join(unknown, "\n  "))
	}
	return nil
}

// envVars maps a Go field path ("Server.BaseURL") to the variable that sets it.
//
// It reads the calls rather than deriving the name, because the pairing in
// applyEnv is the only place that says which variable sets which field.
func envVars(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != gotoken.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(name, "PANORAMA_") {
				return true
			}
			unary, ok := call.Args[1].(*ast.UnaryExpr)
			if !ok || unary.Op != gotoken.AND {
				return true
			}
			if path := selectorPath(unary.X); path != "" {
				out[path] = name
			}
			return true
		})
	}
	return out
}

// selectorPath renders `cfg.Server.BaseURL` as "Server.BaseURL".
func selectorPath(e ast.Expr) string {
	var parts []string
	for {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			break
		}
		parts = append([]string{sel.Sel.Name}, parts...)
		e = sel.X
	}
	if _, ok := e.(*ast.Ident); !ok || len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ".")
}

func walk(
	structs map[string]*ast.StructType,
	typeName, yamlPrefix, goPrefix, section string,
	env map[string]string,
	defaults reflect.Value,
	out *[]Key,
) {
	st, ok := structs[typeName]
	if !ok {
		return
	}

	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		name := field.Names[0].Name
		if !ast.IsExported(name) {
			continue
		}

		tag := yamlTag(field)
		if tag == "-" || tag == "" {
			continue
		}

		goPath := name
		if goPrefix != "" {
			goPath = goPrefix + "." + name
		}
		yamlPath := tag
		if yamlPrefix != "" {
			yamlPath = yamlPrefix + "." + tag
		}
		at := section
		if at == "" {
			at = tag
		}

		value := fieldValue(defaults, name)

		// A nested struct is a section, not a key - unless it is a named type
		// this generator has no struct for, which is a leaf like Decimal.
		if nested := structName(field.Type); nested != "" {
			if _, isStruct := structs[nested]; isStruct {
				walk(structs, nested, yamlPath, goPath, at, env, value, out)
				continue
			}
		}

		*out = append(*out, Key{
			Path:    yamlPath,
			Name:    name,
			Env:     env[goPath],
			Type:    renderType(field.Type, value),
			Default: renderDefault(value),
			Doc:     strings.TrimSpace(field.Doc.Text()),
			Section: at,
		})
	}
}

func yamlTag(f *ast.Field) string {
	if f.Tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return ""
	}
	tag := reflect.StructTag(raw).Get("yaml")
	if tag == "" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

func structName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func fieldValue(v reflect.Value, name string) reflect.Value {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v.FieldByName(name)
}

func renderType(e ast.Expr, v reflect.Value) string {
	if v.IsValid() && v.Kind() == reflect.Pointer {
		// A pointer is how a field says "unset is different from false". The
		// reader does not care about the indirection, only the type.
		v = reflect.New(v.Type().Elem()).Elem()
	}
	if v.IsValid() {
		if v.Type().String() == "time.Duration" {
			return "duration"
		}
		switch v.Kind() {
		case reflect.Bool:
			return "boolean"
		case reflect.Slice:
			return "list"
		case reflect.Map:
			return "map"
		case reflect.Int, reflect.Int64:
			return "number"
		case reflect.String:
			return "string"
		}
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "string"
}

// renderDefault says what a deployment that sets nothing gets.
//
// An empty value is "not set" rather than `""` or `0`: those read as values
// somebody chose, and the distinction between "defaults to zero" and "has no
// default" is the one a reader of this table is actually checking.
func renderDefault(v reflect.Value) string {
	if !v.IsValid() {
		return "not set"
	}
	if v.Kind() == reflect.Pointer {
		// A nil pointer is a field left alone, and what that means is in the
		// field's own comment - "on by default", "zero uses the default" -
		// which the table carries beside it. Inventing a value here would put
		// a figure in the column that no code ever reads.
		if v.IsNil() {
			return "not set"
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		if v.String() == "" {
			return "not set"
		}
		return v.String()
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	case reflect.Int, reflect.Int64:
		if d, ok := v.Interface().(time.Duration); ok {
			if d == 0 {
				return "not set"
			}
			// Go renders 24h as "24h0m0s"; a table wants "24h". Only a zero
			// component is dropped, and only when a larger unit precedes it -
			// trimming a bare "0s" suffix turns 30s into 3, which is the bug
			// this comment exists to stop somebody reintroducing.
			out := d.String()
			if strings.HasSuffix(out, "m0s") {
				out = strings.TrimSuffix(out, "0s")
			}
			if strings.HasSuffix(out, "h0m") {
				out = strings.TrimSuffix(out, "0m")
			}
			return out
		}
		n := v.Int()
		if n == 0 {
			return "not set"
		}
		// Byte sizes read better as MiB than as eight digits.
		if n >= 1<<20 && n%(1<<20) == 0 {
			return fmt.Sprintf("%d MiB", n/(1<<20))
		}
		return strconv.FormatInt(n, 10)
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			return "empty"
		}
		return fmt.Sprintf("%v", v.Interface())
	}
	return fmt.Sprintf("%v", v.Interface())
}

// SortSections returns the section names in the order they were collected,
// which is the file's order.
func SortSections(keys []Key) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if !seen[k.Section] {
			seen[k.Section] = true
			out = append(out, k.Section)
		}
	}
	return out
}

// Dir is the package directory this generator reads, relative to the module
// root.
func Dir(root string) string { return filepath.Join(root, "internal", "config") }

// Marshal renders keys as the JSON the docs read, with a trailing newline so
// the file is a well-formed text file and a diff of it is one line per change.
func Marshal(keys []Key) ([]byte, error) {
	body, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
