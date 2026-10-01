package contract

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// parseYAML reads the YAML subset future_kids_api.yaml is written in (block mappings and
// sequences, flow sequences of scalars, quoted and plain scalars, literal block scalars) into
// map[string]any, []any, string, json.Number, bool and nil. Anything outside that subset, or
// any plain scalar another YAML parser could read differently, is an error: the contract is
// never checked against a misread spec.
func parseYAML(src string) (any, error) {
	p := &yamlParser{lines: strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")}
	for i, l := range p.lines {
		if strings.HasPrefix(l, "---") || strings.HasPrefix(l, "...") || strings.HasPrefix(l, "%") {
			return nil, fmt.Errorf("line %d: documents and directives are not supported", i+1)
		}
		if strings.HasPrefix(strings.TrimLeft(l, " "), "\t") {
			return nil, fmt.Errorf("line %d: tab in indentation", i+1)
		}
	}
	ind, _, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("empty document")
	}
	if ind != 0 {
		return nil, fmt.Errorf("line %d: document must start at column 0", p.i+1)
	}
	v, err := p.block(0)
	if err != nil {
		return nil, err
	}
	if _, _, ok := p.peek(); ok {
		return nil, fmt.Errorf("line %d: unexpected content after the document", p.i+1)
	}
	return v, nil
}

type yamlParser struct {
	lines []string
	i     int
}

func (p *yamlParser) peek() (int, string, bool) {
	for p.i < len(p.lines) {
		l := strings.TrimRight(p.lines[p.i], " ")
		t := strings.TrimLeft(l, " ")
		if t == "" || strings.HasPrefix(t, "#") {
			p.i++
			continue
		}
		return len(l) - len(t), t, true
	}
	return 0, "", false
}

func (p *yamlParser) errf(format string, args ...any) error {
	return fmt.Errorf("line %d: %s", p.i+1, fmt.Sprintf(format, args...))
}

func (p *yamlParser) block(ind int) (any, error) {
	_, t, _ := p.peek()
	if t == "-" || strings.HasPrefix(t, "- ") {
		return p.seq(ind)
	}
	return p.mapping(ind)
}

func (p *yamlParser) mapping(ind int) (any, error) {
	m := map[string]any{}
	for {
		i, t, ok := p.peek()
		if !ok || i < ind {
			return m, nil
		}
		if i > ind {
			return nil, p.errf("unexpected indentation")
		}
		key, rest, isKey, err := splitKey(t)
		if err != nil {
			return nil, p.errf("%v", err)
		}
		if !isKey {
			return nil, p.errf("expected a mapping key, got %q", t)
		}
		if _, dup := m[key]; dup {
			return nil, p.errf("duplicate key %q", key)
		}
		p.i++
		v, err := p.value(rest, ind)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
}

func (p *yamlParser) seq(ind int) (any, error) {
	s := []any{}
	for {
		i, t, ok := p.peek()
		if !ok || i < ind {
			return s, nil
		}
		if i > ind {
			return nil, p.errf("unexpected indentation")
		}
		if t != "-" && !strings.HasPrefix(t, "- ") {
			return nil, p.errf("expected a sequence item, got %q", t)
		}
		item := strings.TrimLeft(strings.TrimPrefix(t, "-"), " ")
		if item == "" {
			p.i++
			j, _, ok := p.peek()
			if !ok || j <= ind {
				return nil, p.errf("empty sequence item")
			}
			v, err := p.block(j)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
			continue
		}
		if item == "-" || strings.HasPrefix(item, "- ") {
			p.lines[p.i] = strings.Repeat(" ", ind+2) + item
			v, err := p.seq(ind + 2)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
			continue
		}
		if _, _, isKey, err := splitKey(item); err != nil {
			return nil, p.errf("%v", err)
		} else if isKey {
			p.lines[p.i] = strings.Repeat(" ", ind+2) + item
			v, err := p.mapping(ind + 2)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
			continue
		}
		p.i++
		v, err := p.inline(item)
		if err != nil {
			return nil, p.errf("%v", err)
		}
		s = append(s, v)
	}
}

func (p *yamlParser) value(rest string, ind int) (any, error) {
	if rest == "" || strings.HasPrefix(rest, "#") {
		j, t, ok := p.peek()
		if !ok || j < ind || (j == ind && !strings.HasPrefix(t, "- ")) {
			return nil, p.errf("empty value; write null explicitly")
		}
		if j == ind {
			return nil, p.errf("a sequence must be indented under its key")
		}
		return p.block(j)
	}
	if rest == "|" || rest == "|-" {
		return p.literal(ind, rest == "|-")
	}
	if strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, ">") {
		return nil, p.errf("block scalar style %q is not supported", rest)
	}
	v, err := p.inline(rest)
	if err != nil {
		return nil, fmt.Errorf("line %d: %v", p.i, err)
	}
	return v, nil
}

func (p *yamlParser) literal(ind int, strip bool) (any, error) {
	var out []string
	content := -1
	for p.i < len(p.lines) {
		l := strings.TrimRight(p.lines[p.i], " ")
		t := strings.TrimLeft(l, " ")
		if t == "" {
			out = append(out, "")
			p.i++
			continue
		}
		n := len(l) - len(t)
		if n <= ind {
			break
		}
		if content < 0 {
			content = n
		}
		if n < content {
			return nil, p.errf("literal block line is less indented than its first line")
		}
		out = append(out, l[content:])
		p.i++
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if content < 0 {
		return nil, p.errf("empty literal block")
	}
	s := strings.Join(out, "\n")
	if !strip {
		s += "\n"
	}
	return s, nil
}

var (
	yamlIntRE      = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	yamlFloatRE    = regexp.MustCompile(`^-?(0|[1-9][0-9]*)\.[0-9]+$`)
	yamlNumberish  = regexp.MustCompile(`^[-+.]?[0-9]`)
	yamlDateish    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}`)
	yamlBoolish    = map[string]bool{"yes": true, "no": true, "on": true, "off": true, "y": true, "n": true}
	yamlPlainStart = "&*!%@`|>?{}[],\"'"
)

func splitKey(t string) (key, rest string, isKey bool, err error) {
	if t[0] == '\'' || t[0] == '"' {
		s, n, err := quoted(t)
		if err != nil {
			return "", "", false, err
		}
		after := t[n:]
		if after == ":" || strings.HasPrefix(after, ": ") {
			return s, strings.TrimSpace(strings.TrimPrefix(after, ":")), true, nil
		}
		return "", "", false, nil
	}
	if t[0] == '[' || t[0] == '{' {
		return "", "", false, nil
	}
	for i := 0; i < len(t); i++ {
		if t[i] == '#' && i > 0 && t[i-1] == ' ' {
			return "", "", false, nil
		}
		if t[i] == ':' && (i == len(t)-1 || t[i+1] == ' ') {
			k := strings.TrimSpace(t[:i])
			if k == "" || strings.ContainsAny(k[:1], yamlPlainStart) || strings.HasPrefix(k, "- ") {
				return "", "", false, fmt.Errorf("unsupported key %q", k)
			}
			return k, strings.TrimSpace(t[i+1:]), true, nil
		}
	}
	return "", "", false, nil
}

func (p *yamlParser) inline(t string) (any, error) {
	switch t[0] {
	case '\'', '"':
		s, n, err := quoted(t)
		if err != nil {
			return nil, err
		}
		if tail := strings.TrimSpace(t[n:]); tail != "" && !strings.HasPrefix(tail, "#") {
			return nil, fmt.Errorf("unexpected text after quoted scalar: %q", tail)
		}
		return s, nil
	case '[':
		return flowSeq(t)
	case '{':
		if strings.TrimSpace(stripComment(t)) == "{}" {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("flow mappings are not supported: %q", t)
	}
	return plain(stripComment(t))
}

func stripComment(t string) string {
	if i := strings.Index(t, " #"); i >= 0 {
		return strings.TrimSpace(t[:i])
	}
	return t
}

func plain(t string) (any, error) {
	if strings.ContainsAny(t[:1], yamlPlainStart) || strings.HasPrefix(t, "- ") {
		return nil, fmt.Errorf("plain scalar %q starts with an indicator; quote it", t)
	}
	if strings.Contains(t, ": ") || strings.HasSuffix(t, ":") {
		return nil, fmt.Errorf("plain scalar %q contains ': '; quote it", t)
	}
	switch {
	case t == "null" || t == "~":
		return nil, nil
	case t == "true":
		return true, nil
	case t == "false":
		return false, nil
	case yamlBoolish[strings.ToLower(t)] || strings.EqualFold(t, "true") || strings.EqualFold(t, "false") || strings.EqualFold(t, "null"):
		return nil, fmt.Errorf("plain scalar %q is ambiguous; quote it or write true/false/null", t)
	case yamlIntRE.MatchString(t) || yamlFloatRE.MatchString(t):
		return json.Number(t), nil
	case yamlDateish.MatchString(t):
		return nil, fmt.Errorf("plain scalar %q looks like a date; quote it", t)
	case yamlNumberish.MatchString(t) && !strings.ContainsAny(t, " \t"):
		if _, err := strconv.ParseFloat(t, 64); err == nil || strings.HasPrefix(t, "0") {
			return nil, fmt.Errorf("plain scalar %q is an ambiguous number; quote it", t)
		}
	}
	return t, nil
}

func quoted(t string) (string, int, error) {
	q := t[0]
	var b strings.Builder
	for i := 1; i < len(t); i++ {
		c := t[i]
		if q == '\'' {
			if c == '\'' {
				if i+1 < len(t) && t[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return b.String(), i + 1, nil
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(t) {
				return "", 0, fmt.Errorf("unterminated escape in %q", t)
			}
			i++
			switch t[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\', '/':
				b.WriteByte(t[i])
			case 'u':
				if i+4 >= len(t) {
					return "", 0, fmt.Errorf("short \\u escape in %q", t)
				}
				r, err := strconv.ParseUint(t[i+1:i+5], 16, 32)
				if err != nil {
					return "", 0, fmt.Errorf("bad \\u escape in %q", t)
				}
				b.WriteRune(rune(r))
				i += 4
			default:
				return "", 0, fmt.Errorf("unsupported escape \\%c in %q", t[i], t)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, fmt.Errorf("unterminated quoted scalar %q", t)
}

func flowSeq(t string) (any, error) {
	t = strings.TrimSpace(t)
	end := -1
	inQ := byte(0)
	for i := 1; i < len(t); i++ {
		c := t[i]
		switch {
		case inQ != 0:
			if c == inQ {
				inQ = 0
			}
		case c == '\'' || c == '"':
			inQ = c
		case c == '[' || c == '{':
			return nil, fmt.Errorf("nested flow collections are not supported: %q", t)
		case c == ']':
			end = i
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("unterminated flow sequence %q", t)
	}
	if tail := strings.TrimSpace(t[end+1:]); tail != "" && !strings.HasPrefix(tail, "#") {
		return nil, fmt.Errorf("unexpected text after flow sequence: %q", tail)
	}
	body := strings.TrimSpace(t[1:end])
	out := []any{}
	if body == "" {
		return out, nil
	}
	var parts []string
	start, inQ := 0, byte(0)
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inQ != 0:
			if c == inQ {
				inQ = 0
			}
		case c == '\'' || c == '"':
			inQ = c
		case c == ',':
			parts = append(parts, body[start:i])
			start = i + 1
		}
	}
	parts = append(parts, body[start:])
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty item in flow sequence %q", t)
		}
		var v any
		var err error
		if part[0] == '\'' || part[0] == '"' {
			var n int
			var s string
			s, n, err = quoted(part)
			if err == nil && n != len(part) {
				err = fmt.Errorf("unexpected text after quoted item %q", part)
			}
			v = s
		} else {
			v, err = plain(part)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func TestYAMLSubsetParser(t *testing.T) {
	src := "a: 1\n" +
		"b: 'it''s: fine'\n" +
		"'200':\n" +
		"  description: x # comment\n" +
		"list:\n" +
		"  - name: id\n" +
		"    in: query\n" +
		"  - plain item\n" +
		"flow: [a, 'b, c', \"d\"]\n" +
		"empty: []\n" +
		"obj: {}\n" +
		"text: |\n" +
		"  line one\n" +
		"\n" +
		"    indented\n" +
		"n: null\n" +
		"f: false\n" +
		"s: \"tab\\there\"\n" +
		"ar: الأحد\n" +
		"nested:\n" +
		"  - - x\n"
	got, err := parseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": json.Number("1"), "b": "it's: fine",
		"200":   map[string]any{"description": "x"},
		"list":  []any{map[string]any{"name": "id", "in": "query"}, "plain item"},
		"flow":  []any{"a", "b, c", "d"},
		"empty": []any{}, "obj": map[string]any{},
		"text": "line one\n\n  indented\n",
		"n":    nil, "f": false, "s": "tab\there", "ar": "الأحد",
		"nested": []any{[]any{"x"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed\n%#v\nwant\n%#v", got, want)
	}

	for _, bad := range []string{
		"a: 2026-09-21\n",
		"a: yes\n",
		"a: 0x1F\n",
		"a: b: c\n",
		"a: &anchor x\n",
		"a: *alias\n",
		"a: {b: 1}\n",
		"a: >\n  folded\n",
		"a:\n",
		"a: 1\na: 2\n",
		"a:\n- x\n",
		"a: [x, [y]]\n",
		"---\na: 1\n",
		"a: 1\n   b: 2\n",
		"a: `x`\n",
		"a: 'unterminated\n",
	} {
		if _, err := parseYAML(bad); err == nil {
			t.Errorf("parseYAML accepted unsupported input %q", bad)
		}
	}
}
