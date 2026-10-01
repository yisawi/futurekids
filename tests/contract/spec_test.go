package contract

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	stagingURL   = "https://futurekids-staging.up.railway.app"
	localURL     = "http://localhost:8080"
	internalTag  = "Hardware (internal)"
	contractSpec = "future_kids_api.yaml"
)

var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

type apiSpec struct {
	root map[string]any
}

type operation struct {
	method, path, id string
	op               map[string]any
}

func (o operation) key() string { return strings.ToUpper(o.method) + " " + o.path }

func parseSpec(raw string) (*apiSpec, error) {
	v, err := parseYAML(raw)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("spec root is not a mapping")
	}
	return &apiSpec{root: root}, nil
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s *apiSpec) operations() []operation {
	var ops []operation
	paths := obj(s.root["paths"])
	for _, p := range sortedKeys(paths) {
		item := obj(paths[p])
		for _, m := range httpMethods {
			if op := obj(item[m]); op != nil {
				ops = append(ops, operation{method: m, path: p, id: str(op["operationId"]), op: op})
			}
		}
	}
	return ops
}

func (s *apiSpec) operation(id string) (operation, bool) {
	for _, o := range s.operations() {
		if o.id == id {
			return o, true
		}
	}
	return operation{}, false
}

func (s *apiSpec) lookup(ref string) (map[string]any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("only local $refs are supported: %q", ref)
	}
	var cur any = s.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m := obj(cur)
		if m == nil {
			return nil, fmt.Errorf("$ref %q does not resolve", ref)
		}
		next, ok := m[part]
		if !ok {
			return nil, fmt.Errorf("$ref %q does not resolve", ref)
		}
		cur = next
	}
	m := obj(cur)
	if m == nil {
		return nil, fmt.Errorf("$ref %q does not point to an object", ref)
	}
	return m, nil
}

func (s *apiSpec) deref(node map[string]any) (map[string]any, error) {
	for i := 0; i < 16 && node != nil; i++ {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node, nil
		}
		next, err := s.lookup(ref)
		if err != nil {
			return nil, err
		}
		node = next
	}
	return nil, fmt.Errorf("$ref chain too deep")
}

var docKeywords = map[string]bool{
	"description": true, "example": true, "examples": true, "default": true, "title": true,
	"deprecated": true, "readOnly": true, "writeOnly": true,
}

// validate checks value against schema. Objects that declare properties are closed: a property
// the schema does not declare is reported, so an undocumented field fails the contract.
func (s *apiSpec) validate(schema map[string]any, value any, at string) []string {
	schema, err := s.deref(schema)
	if err != nil {
		return []string{at + ": " + err.Error()}
	}
	var problems []string
	bad := func(format string, args ...any) {
		problems = append(problems, at+": "+fmt.Sprintf(format, args...))
	}
	for k := range schema {
		switch k {
		case "type", "nullable", "properties", "required", "additionalProperties", "items", "enum",
			"format", "pattern", "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems":
		default:
			if !docKeywords[k] && !strings.HasPrefix(k, "x-") {
				bad("schema keyword %q is not supported by the contract validator", k)
			}
		}
	}
	if value == nil {
		if schema["nullable"] != true {
			bad("is null, but the schema is not nullable")
		}
		return problems
	}
	typ := str(schema["type"])
	switch typ {
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			bad("is %s, want object", jsonKind(value))
			return problems
		}
		props := obj(schema["properties"])
		for _, r := range list(schema["required"]) {
			if _, ok := m[str(r)]; !ok {
				bad("required property %q is missing", str(r))
			}
		}
		addl, hasAddl := schema["additionalProperties"]
		for _, k := range sortedKeys(m) {
			if ps, ok := props[k]; ok {
				problems = append(problems, s.validate(obj(ps), m[k], at+"."+k)...)
				continue
			}
			switch a := addl.(type) {
			case map[string]any:
				problems = append(problems, s.validate(a, m[k], at+"."+k)...)
			case bool:
				if !a {
					bad("property %q is not in the schema", k)
				}
			default:
				if !hasAddl && props != nil {
					bad("property %q is not in the schema", k)
				}
			}
		}
	case "array":
		l, ok := value.([]any)
		if !ok {
			bad("is %s, want array", jsonKind(value))
			return problems
		}
		if n, ok := number(schema["maxItems"]); ok && float64(len(l)) > n {
			bad("has %d items, more than maxItems %v", len(l), n)
		}
		if n, ok := number(schema["minItems"]); ok && float64(len(l)) < n {
			bad("has %d items, fewer than minItems %v", len(l), n)
		}
		if items := obj(schema["items"]); items != nil {
			for i, it := range l {
				problems = append(problems, s.validate(items, it, fmt.Sprintf("%s[%d]", at, i))...)
			}
		} else {
			bad("array schema has no items")
		}
	case "string":
		v, ok := value.(string)
		if !ok {
			bad("is %s, want string", jsonKind(value))
			return problems
		}
		if n, ok := number(schema["minLength"]); ok && float64(len([]rune(v))) < n {
			bad("%q is shorter than minLength %v", v, n)
		}
		if n, ok := number(schema["maxLength"]); ok && float64(len([]rune(v))) > n {
			bad("%q is longer than maxLength %v", v, n)
		}
		if pat, ok := schema["pattern"].(string); ok {
			re, err := regexp.Compile(pat)
			if err != nil {
				bad("pattern %q does not compile: %v", pat, err)
			} else if !re.MatchString(v) {
				bad("%q does not match pattern %s", v, pat)
			}
		}
		switch f := str(schema["format"]); f {
		case "":
		case "date":
			if _, err := time.Parse("2006-01-02", v); err != nil {
				bad("%q is not a date (YYYY-MM-DD)", v)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				bad("%q is not an RFC 3339 date-time", v)
			}
		case "binary":
		default:
			bad("string format %q is not supported by the contract validator", f)
		}
	case "integer", "number":
		n, ok := value.(json.Number)
		if !ok {
			bad("is %s, want %s", jsonKind(value), typ)
			return problems
		}
		f, err := n.Float64()
		if err != nil {
			bad("%q is not a number", n)
			return problems
		}
		if typ == "integer" {
			i, err := n.Int64()
			if err != nil {
				bad("%s is not an integer", n)
				return problems
			}
			switch fm := str(schema["format"]); fm {
			case "", "int64":
			case "int32":
				if i < math.MinInt32 || i > math.MaxInt32 {
					bad("%d is outside int32", i)
				}
			default:
				bad("integer format %q is not supported by the contract validator", fm)
			}
		}
		if m, ok := number(schema["minimum"]); ok && f < m {
			bad("%s is below minimum %v", n, m)
		}
		if m, ok := number(schema["maximum"]); ok && f > m {
			bad("%s is above maximum %v", n, m)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			bad("is %s, want boolean", jsonKind(value))
			return problems
		}
	default:
		bad("schema has no supported type (%q)", typ)
		return problems
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(value) && jsonKind(e) == jsonKind(value) {
				found = true
			}
		}
		if !found {
			bad("%v is not one of %v", value, enum)
		}
	}
	return problems
}

func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	}
	return fmt.Sprintf("%T", v)
}

// mediaExamples returns the examples a media type object documents, from the media type itself
// or, failing that, from its schema.
func (s *apiSpec) mediaExamples(media map[string]any) ([]any, error) {
	if ex, ok := media["example"]; ok {
		return []any{ex}, nil
	}
	if exs := obj(media["examples"]); exs != nil {
		var out []any
		for _, k := range sortedKeys(exs) {
			e, err := s.deref(obj(exs[k]))
			if err != nil {
				return nil, err
			}
			v, ok := e["value"]
			if !ok {
				return nil, fmt.Errorf("example %q has no value", k)
			}
			out = append(out, v)
		}
		return out, nil
	}
	schema, err := s.deref(obj(media["schema"]))
	if err != nil {
		return nil, err
	}
	if ex, ok := schema["example"]; ok {
		return []any{ex}, nil
	}
	return nil, nil
}

// hygieneProblems checks the spec on its own: structure, references, examples and the data
// that must never appear in it.
func hygieneProblems(s *apiSpec) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if v := str(s.root["openapi"]); !strings.HasPrefix(v, "3.0.") {
		bad("openapi is %q, want 3.0.x (RULES.md §4)", v)
	}
	var urls []string
	for _, sv := range list(s.root["servers"]) {
		urls = append(urls, str(obj(sv)["url"]))
	}
	if !reflect.DeepEqual(urls, []string{stagingURL, localURL}) {
		bad("servers are %v, want exactly [%s %s]", urls, stagingURL, localURL)
	}
	for _, u := range urls {
		if strings.Contains(strings.ToLower(u), "prod") {
			bad("servers must not list production: %s", u)
		}
	}

	tags := map[string]bool{}
	for _, tg := range list(s.root["tags"]) {
		tags[str(obj(tg)["name"])] = true
	}
	ids := map[string]string{}
	for _, o := range s.operations() {
		at := o.key()
		if o.id == "" {
			bad("%s has no operationId", at)
		} else if prev, dup := ids[o.id]; dup {
			bad("%s and %s share operationId %q", prev, at, o.id)
		}
		ids[o.id] = at
		if str(o.op["summary"]) == "" {
			bad("%s has no summary", at)
		}
		opTags := list(o.op["tags"])
		if len(opTags) == 0 {
			bad("%s has no tags", at)
		}
		for _, tg := range opTags {
			if !tags[str(tg)] {
				bad("%s uses tag %q, which is not declared", at, str(tg))
			}
		}
		internal := len(opTags) == 1 && str(opTags[0]) == internalTag
		if (o.op["x-internal"] == true) != internal {
			bad("%s: x-internal must be true exactly for %q operations", at, internalTag)
		}
		if _, ok := o.op["security"]; !ok {
			bad("%s has no security (use [] for a public operation)", at)
		}
		responses := obj(o.op["responses"])
		if len(responses) == 0 {
			bad("%s has no responses", at)
		}
		success := false
		for _, code := range sortedKeys(responses) {
			if strings.HasPrefix(code, "2") {
				success = true
			}
			r, err := s.deref(obj(responses[code]))
			if err != nil {
				bad("%s %s: %v", at, code, err)
				continue
			}
			if str(r["description"]) == "" {
				bad("%s %s has no description", at, code)
			}
			problems = append(problems, s.mediaProblems(fmt.Sprintf("%s %s", at, code), obj(r["content"]))...)
			for _, h := range sortedKeys(obj(r["headers"])) {
				hd, err := s.deref(obj(obj(r["headers"])[h]))
				if err != nil {
					bad("%s %s header %s: %v", at, code, h, err)
				} else if ex, ok := hd["example"]; ok {
					for _, p := range s.validate(obj(hd["schema"]), ex, fmt.Sprintf("%s %s header %s example", at, code, h)) {
						bad("%s", p)
					}
				}
			}
		}
		if !success {
			bad("%s documents no 2xx response", at)
		}
		if rb := obj(o.op["requestBody"]); rb != nil {
			problems = append(problems, s.mediaProblems(at+" request", obj(rb["content"]))...)
		}
		for _, pr := range list(o.op["parameters"]) {
			p, err := s.deref(obj(pr))
			if err != nil {
				bad("%s parameter: %v", at, err)
				continue
			}
			ex, ok := p["example"]
			if !ok {
				bad("%s parameter %s has no example", at, str(p["name"]))
				continue
			}
			for _, e := range s.validate(obj(p["schema"]), ex, fmt.Sprintf("%s parameter %s example", at, str(p["name"]))) {
				bad("%s", e)
			}
		}
	}

	refs := map[string]bool{}
	walk(s.root, func(v any) {
		if m := obj(v); m != nil {
			if ref, ok := m["$ref"].(string); ok {
				refs[ref] = true
				if _, err := s.lookup(ref); err != nil {
					bad("%v", err)
				}
			}
		}
	})
	components := obj(s.root["components"])
	for _, kind := range []string{"schemas", "responses", "parameters", "headers"} {
		for _, name := range sortedKeys(obj(components[kind])) {
			if !refs["#/components/"+kind+"/"+name] {
				bad("components/%s/%s is never used", kind, name)
			}
		}
	}
	usedSchemes := map[string]bool{}
	for _, o := range s.operations() {
		for _, req := range list(o.op["security"]) {
			for name := range obj(req) {
				usedSchemes[name] = true
				if obj(obj(components["securitySchemes"])[name]) == nil {
					bad("%s uses undeclared security scheme %q", o.key(), name)
				}
			}
		}
	}
	for _, name := range sortedKeys(obj(components["securitySchemes"])) {
		if !usedSchemes[name] {
			bad("securityScheme %s is never used", name)
		}
	}

	schemas := obj(components["schemas"])
	names := sortedKeys(schemas)
	for i, name := range names {
		sc := obj(schemas[name])
		ex, ok := sc["example"]
		if !ok {
			bad("schema %s has no example", name)
		} else {
			for _, p := range s.validate(sc, ex, "schema "+name+" example") {
				bad("%s", p)
			}
		}
		for _, other := range names[i+1:] {
			if reflect.DeepEqual(structural(sc), structural(obj(schemas[other]))) {
				bad("schemas %s and %s are duplicates", name, other)
			}
		}
	}

	phoneRE := regexp.MustCompile(`\+964[0-9]{6,}`)
	jwtRE := regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+`)
	serialRE := regexp.MustCompile(`\b[A-Z]{3}[0-9]{10}\b`)
	walk(s.root, func(v any) {
		sv, ok := v.(string)
		if !ok {
			return
		}
		for _, ph := range phoneRE.FindAllString(sv, -1) {
			if !strings.HasPrefix(ph, "+96470000") {
				bad("phone number %s could be real; use the +96470000… example block", ph)
			}
		}
		for _, tok := range jwtRE.FindAllString(sv, -1) {
			if !strings.Contains(tok, "EXAMPLE") {
				bad("token %.20s... looks real; example tokens must contain EXAMPLE", tok)
			}
		}
		if serialRE.MatchString(sv) {
			bad("%q looks like a real device serial number; use TEST-SN-...", sv)
		}
		if strings.Contains(sv, "admin123") {
			bad("the spec contains the default admin password")
		}
	})
	return problems
}

func (s *apiSpec) mediaProblems(at string, content map[string]any) []string {
	var problems []string
	if len(content) == 0 && !strings.HasSuffix(at, "request") {
		return nil
	}
	for _, mt := range sortedKeys(content) {
		media := obj(content[mt])
		schema := obj(media["schema"])
		if schema == nil {
			problems = append(problems, fmt.Sprintf("%s %s has no schema", at, mt))
			continue
		}
		resolved, err := s.deref(schema)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", at, mt, err))
			continue
		}
		if str(resolved["format"]) == "binary" {
			continue
		}
		exs, err := s.mediaExamples(media)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", at, mt, err))
			continue
		}
		if len(exs) == 0 {
			problems = append(problems, fmt.Sprintf("%s %s has no example", at, mt))
		}
		for _, ex := range exs {
			problems = append(problems, s.validate(schema, ex, fmt.Sprintf("%s %s example", at, mt))...)
		}
	}
	return problems
}

func structural(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if k == "description" || k == "example" || k == "examples" || k == "title" {
				continue
			}
			out[k] = structural(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = structural(t[i])
		}
		return out
	}
	return v
}

func walk(v any, fn func(any)) {
	fn(v)
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			walk(t[k], fn)
		}
	case []any:
		for _, e := range t {
			walk(e, fn)
		}
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = deepCopy(t[i])
		}
		return out
	}
	return v
}
