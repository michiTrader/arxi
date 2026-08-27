// Package blueprint loads, validates and freezes blueprints.
//
// It contains a YAML parser written from scratch, which needs justifying
// because writing a parser is normally the wrong answer. Two facts force it:
// blueprints are YAML (docs/design/20-use-cases.md §20.4, ADR-0002), and the
// project ships as a single static binary with no runtime, which AGENTS.md
// counts as a feature and every dependency as a claim against it.
//
// The honest risk of a hand-written parser is not that it fails to parse
// something — that is loud and fixable. It is that it *misparses* something and
// hands back a config that looks plausible and is wrong, and the user then
// debugs a run whose rules are not the rules they wrote. So this parser accepts
// a deliberately small subset and **refuses everything else by name**, with the
// line, the construct and what to write instead. There is no fallback path that
// guesses.
package blueprint

import (
	"fmt"
	"strconv"
	"strings"
)

// SyntaxError says where the problem is and what to do about it.
//
// Hint is not decoration. A parser that supports a subset will reject valid
// YAML, and a bare "unexpected character" leaves the user assuming their file
// is malformed when it is merely unsupported. Those are different problems with
// different fixes, and the message has to distinguish them.
type SyntaxError struct {
	Line int
	Msg  string
	Hint string
}

func (e *SyntaxError) Error() string {
	s := fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	if e.Hint != "" {
		s += "\n  " + e.Hint
	}
	return s
}

func errAt(line int, msg, hint string) error {
	return &SyntaxError{Line: line, Msg: msg, Hint: hint}
}

// Parse reads the supported YAML subset and returns map[string]any, []any,
// string, int64, float64, bool or nil.
//
// It returns any rather than decoding straight into kernel.Config so that
// unknown keys can be reported as unknown (see decode). Unmarshalling directly
// into a struct would silently drop a misspelled `advance_when`, and a
// misspelled advance rule is exactly the kind of mistake that surfaces hours
// later as a run that will not advance.
func Parse(src []byte) (any, error) {
	lines, err := scan(string(src))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	v, next, err := parseBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if next < len(lines) {
		return nil, errAt(lines[next].num, "unexpected content after the end of the document",
			"check the indentation: this line is less indented than the block it belongs to")
	}
	return v, nil
}

// line is a source line already stripped of comments, with its indentation
// measured. Blank lines are dropped: they carry no structure in this subset and
// keeping them would force every rule below to special-case them.
type line struct {
	num    int
	indent int
	text   string
}

func scan(src string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(src, "\n") {
		num := i + 1
		raw = strings.TrimRight(raw, "\r")

		if strings.HasPrefix(strings.TrimSpace(raw), "---") || strings.HasPrefix(strings.TrimSpace(raw), "...") {
			return nil, errAt(num, "multi-document YAML is not supported",
				"a blueprint is one document; remove the `---` separator")
		}

		indent := 0
		for indent < len(raw) && (raw[indent] == ' ' || raw[indent] == '\t') {
			if raw[indent] == '\t' {
				// YAML forbids tabs in indentation, and the failure it produces
				// otherwise is the worst kind: the file looks correctly aligned
				// in the author's editor and nests differently here.
				return nil, errAt(num, "tab used for indentation",
					"YAML forbids tabs for indentation; use spaces")
			}
			indent++
		}

		text, err := stripComment(raw[indent:], num)
		if err != nil {
			return nil, err
		}
		text = strings.TrimRight(text, " ")
		if text == "" {
			continue
		}
		out = append(out, line{num: num, indent: indent, text: text})
	}
	return out, nil
}

// stripComment removes a trailing `#` comment while respecting quotes, because
// `question: "why? # not sure"` is a string, not a comment.
func stripComment(s string, num int) (string, error) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || s[i-1] == ' '):
			return s[:i], nil
		}
	}
	if quote != 0 {
		return "", errAt(num, "unterminated quoted string", "add the closing "+string(quote))
	}
	return s, nil
}

// parseBlock parses the block starting at lines[i] with the given indentation
// and returns the value plus the index of the first line that no longer belongs
// to it.
func parseBlock(lines []line, i, indent int) (any, int, error) {
	if i >= len(lines) {
		return nil, i, nil
	}
	if lines[i].indent != indent {
		return nil, i, errAt(lines[i].num, "unexpected indentation",
			fmt.Sprintf("this line is indented %d spaces; the block it belongs to is at %d",
				lines[i].indent, indent))
	}
	if strings.HasPrefix(lines[i].text, "- ") || lines[i].text == "-" {
		return parseSeq(lines, i, indent)
	}
	return parseMap(lines, i, indent)
}

func parseSeq(lines []line, i, indent int) (any, int, error) {
	out := []any{}
	for i < len(lines) && lines[i].indent == indent {
		l := lines[i]
		if !strings.HasPrefix(l.text, "- ") && l.text != "-" {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l.text, "-"))

		if rest == "" {
			// `-` alone: the item is the indented block underneath.
			if i+1 >= len(lines) || lines[i+1].indent <= indent {
				return nil, i, errAt(l.num, "list item with no value",
					"write the value on the same line, or indent it underneath")
			}
			v, next, err := parseBlock(lines, i+1, lines[i+1].indent)
			if err != nil {
				return nil, i, err
			}
			out = append(out, v)
			i = next
			continue
		}

		// `- key: value` opens a mapping whose first key sits at indent+2. The
		// following keys of the same item are aligned with it rather than with
		// the dash, so the item is parsed as a block map starting on a virtual
		// line at that column.
		if k, _, ok := splitKey(rest); ok && k != "" {
			virt := make([]line, 0, len(lines)-i)
			virt = append(virt, line{num: l.num, indent: indent + 2, text: rest})
			j := i + 1
			for j < len(lines) && lines[j].indent > indent {
				virt = append(virt, lines[j])
				j++
			}
			v, consumed, err := parseMap(virt, 0, indent+2)
			if err != nil {
				return nil, i, err
			}
			if consumed != len(virt) {
				return nil, i, errAt(virt[consumed].num, "unexpected indentation inside the list item",
					"align every key of the item with the first one")
			}
			out = append(out, v)
			i = j
			continue
		}

		v, err := parseScalar(rest, l.num)
		if err != nil {
			return nil, i, err
		}
		out = append(out, v)
		i++
	}
	return out, i, nil
}

func parseMap(lines []line, i, indent int) (any, int, error) {
	out := map[string]any{}
	for i < len(lines) && lines[i].indent == indent {
		l := lines[i]
		if strings.HasPrefix(l.text, "- ") {
			break
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, i, errAt(l.num, fmt.Sprintf("expected `key: value`, found %q", l.text),
				"block scalars (`|`, `>`), anchors (`&`) and tags (`!`) are not supported")
		}
		if key == "" {
			return nil, i, errAt(l.num, "empty key", "every entry needs a name before the `:`")
		}
		if _, dup := out[key]; dup {
			// Real YAML tolerates this and keeps the last one. Here it is an
			// error: a blueprint with `advance_when` twice is a user who
			// believes both apply, and silently honouring one of them produces
			// a run whose rules are not the rules on screen.
			return nil, i, errAt(l.num, fmt.Sprintf("duplicate key %q", key),
				"remove one of the two: keeping the last one silently would hide the mistake")
		}

		if rest != "" {
			v, err := parseScalar(rest, l.num)
			if err != nil {
				return nil, i, err
			}
			out[key] = v
			i++
			continue
		}

		// Empty value: either an indented block below, or null.
		if i+1 < len(lines) && lines[i+1].indent > indent {
			v, next, err := parseBlock(lines, i+1, lines[i+1].indent)
			if err != nil {
				return nil, i, err
			}
			out[key] = v
			i = next
			continue
		}
		// A sequence may be written at the same indentation as its key.
		if i+1 < len(lines) && lines[i+1].indent == indent && strings.HasPrefix(lines[i+1].text, "- ") {
			v, next, err := parseSeq(lines, i+1, indent)
			if err != nil {
				return nil, i, err
			}
			out[key] = v
			i = next
			continue
		}
		out[key] = nil
		i++
	}
	return out, i, nil
}

// splitKey splits `key: rest`, ignoring colons inside quotes and inside flow
// collections so that `tools: [a, b]` and `q: "a: b"` survive.
func splitKey(s string) (key, rest string, ok bool) {
	var quote byte
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ':' && depth == 0:
			if i+1 < len(s) && s[i+1] != ' ' {
				continue // `12:30`, not a key separator
			}
			return strings.TrimSpace(unquoteBare(s[:i])), strings.TrimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}

func unquoteBare(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func parseScalar(s string, num int) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return nil, nil
	case s[0] == '[':
		return parseFlowSeq(s, num)
	case s[0] == '{':
		return parseFlowMap(s, num)
	case s[0] == '&' || s[0] == '*':
		return nil, errAt(num, "anchors and aliases are not supported",
			"write the value out in full; a blueprint that reuses a node by reference is harder to audit than one that repeats it")
	case s[0] == '!':
		return nil, errAt(num, "type tags (`!`) are not supported",
			"remove the tag: types come from the blueprint schema, not from the file")
	case s == "|" || s == ">" || strings.HasPrefix(s, "|-") || strings.HasPrefix(s, ">-"):
		return nil, errAt(num, "block scalars (`|`, `>`) are not supported",
			"put the text on one line between double quotes")
	}

	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		body := s[1 : len(s)-1]
		if s[0] == '\'' {
			return strings.ReplaceAll(body, "''", "'"), nil
		}
		out, err := strconv.Unquote(`"` + body + `"`)
		if err != nil {
			return nil, errAt(num, "invalid escape in a double-quoted string", "check the backslashes")
		}
		return out, nil
	}

	switch s {
	case "true", "false":
		return s == "true", nil
	case "null", "~":
		return nil, nil
	case "yes", "no", "on", "off", "Yes", "No", "On", "Off", "YES", "NO", "ON", "OFF":
		// YAML 1.1 reads these as booleans, YAML 1.2 as strings, and that
		// disagreement is the well-known trap where a member named `no` becomes
		// `false`. Rather than pick a side and be quietly wrong for whoever
		// expected the other, reject it and make the author say which they mean.
		return nil, errAt(num, fmt.Sprintf("%q is ambiguous: YAML 1.1 reads it as a boolean, YAML 1.2 as a string", s),
			"write `true`/`false` for the boolean, or quote it (\""+s+"\") for the string")
	}

	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	return s, nil
}

// splitFlow splits a flow collection body on top-level commas.
func splitFlow(body string, num int, open, close byte) ([]string, error) {
	var parts []string
	var cur strings.Builder
	var quote byte
	depth := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' && i+1 < len(body) {
				cur.WriteByte(c)
				i++
				c = body[i]
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if quote != 0 || depth != 0 {
		return nil, errAt(num, "unbalanced flow collection",
			"check the brackets and the quotes on this line")
	}
	if last := strings.TrimSpace(cur.String()); last != "" {
		parts = append(parts, last)
	}
	return parts, nil
}

func parseFlowSeq(s string, num int) (any, error) {
	if s[len(s)-1] != ']' {
		return nil, errAt(num, "`[` with no closing `]`", "close the list on the same line")
	}
	parts, err := splitFlow(s[1:len(s)-1], num, '[', ']')
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, p := range parts {
		v, err := parseScalar(p, num)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func parseFlowMap(s string, num int) (any, error) {
	if s[len(s)-1] != '}' {
		return nil, errAt(num, "`{` with no closing `}`", "close the mapping on the same line")
	}
	parts, err := splitFlow(s[1:len(s)-1], num, '{', '}')
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, p := range parts {
		k, rest, ok := splitKey(p)
		if !ok || k == "" {
			return nil, errAt(num, fmt.Sprintf("expected `key: value` inside `{...}`, found %q", p),
				"each entry of an inline mapping needs a name and a value")
		}
		if _, dup := out[k]; dup {
			return nil, errAt(num, fmt.Sprintf("duplicate key %q", k),
				"remove one of the two: keeping the last one silently would hide the mistake")
		}
		v, err := parseScalar(rest, num)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}
