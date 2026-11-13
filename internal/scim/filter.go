package scim

// Filters (RFC 7644 section 3.4.2.2) and attribute paths (section 3.5.2).
// Microsoft Entra ID only uses the operators "eq" and "and", e.g.
//
//	userName eq "jane@contoso.com"
//	externalId eq "jdoe" and emails[type eq "work"].value eq "jane@contoso.com"
//
// Other operators, "or", "not" and parentheses are rejected.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// path is an attribute path such as userName, name.givenName,
// emails[type eq "work"].value or members[value eq "42"].
type path struct {
	attr   string // lower case, without the schema prefix
	filter []cond // value filter in brackets; nil if none
	sub    string // sub-attribute, lower case; "" if none
	ext    bool   // attribute of a schema extension (not stored, ignored)
}

func (p path) is(attr, sub string) bool {
	return p.attr == attr && p.sub == sub && p.filter == nil && !p.ext
}

// cond is one comparison of a filter: path eq value.
type cond struct {
	path  path
	value string // compared value: strings unquoted, true/false/null/numbers as written
	str   bool   // value was a quoted string
}

// valueIsTrue reports whether the value is true (or the string "true").
func (c cond) valueIsTrue() bool { return strings.EqualFold(c.value, "true") }

// errFilter is the error for an unusable filter.
func errFilter(format string, args ...any) *apiError {
	return errorf(http.StatusBadRequest, "invalidFilter", "Unsupported filter: "+format, args...)
}

// parseFilter parses a filter of comparisons joined by "and".
func parseFilter(s string) ([]cond, error) {
	l := &lexer{s: s}
	var conds []cond
	for {
		l.skipSpace()
		if l.done() {
			if len(conds) == 0 {
				return nil, errFilter("the filter is empty.")
			}
			return nil, errFilter("%q ends with \"and\".", s)
		}
		attr := l.token()
		switch strings.ToLower(attr) {
		case "not", "(", "not(":
			return nil, errFilter("%q – only comparisons with \"eq\" joined by \"and\" are supported.", s)
		}
		if strings.HasPrefix(attr, "(") || strings.HasPrefix(attr, "\"") {
			return nil, errFilter("%q – only comparisons with \"eq\" joined by \"and\" are supported.", s)
		}
		l.skipSpace()
		op := strings.ToLower(l.token())
		if op == "" {
			return nil, errFilter("%q – the operator is missing after %s.", s, attr)
		}
		if op != "eq" {
			return nil, errFilter("%q – the operator %q is not supported, only \"eq\" (and \"and\" to combine comparisons).", s, op)
		}
		l.skipSpace()
		value, quoted, err := l.value()
		if err != nil {
			return nil, errFilter("%q – %v.", s, err)
		}
		if !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			// The same rule as checkValue for request bodies; PostgreSQL
			// rejects NUL and invalid UTF-8.
			return nil, errFilter("%q – the value contains characters that are not allowed.", s)
		}
		p, err := parsePath(attr)
		if err != nil {
			return nil, errFilter("%q – %v.", s, err)
		}
		conds = append(conds, cond{path: p, value: value, str: quoted})
		l.skipSpace()
		if l.done() {
			return conds, nil
		}
		if kw := l.token(); !strings.EqualFold(kw, "and") {
			return nil, errFilter("%q – %q is not supported, only \"and\" combines comparisons.", s, kw)
		}
	}
}

// lexer splits a filter into tokens.
type lexer struct {
	s   string
	pos int
}

func (l *lexer) done() bool { return l.pos >= len(l.s) }

func (l *lexer) skipSpace() {
	for l.pos < len(l.s) && (l.s[l.pos] == ' ' || l.s[l.pos] == '\t' || l.s[l.pos] == '\r' || l.s[l.pos] == '\n') {
		l.pos++
	}
}

// token reads a word up to the next white space; brackets (with quoted
// strings inside, which may contain spaces) belong to the word.
func (l *lexer) token() string {
	start := l.pos
	depth, quoted := 0, false
	for l.pos < len(l.s) {
		c := l.s[l.pos]
		switch {
		case quoted && c == '\\':
			l.pos++ // skip the escaped character
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '[':
			depth++
		case c == ']':
			depth--
		case depth <= 0 && (c == ' ' || c == '\t' || c == '\r' || c == '\n'):
			return l.s[start:l.pos]
		}
		l.pos++
	}
	l.pos = min(l.pos, len(l.s)) // an escape at the very end skips past it
	return l.s[start:l.pos]
}

// value reads a comparison value: a JSON string, or a word such as true,
// false, null or a number. (Microsoft's examples also show unquoted words.)
func (l *lexer) value() (string, bool, error) {
	if l.done() {
		return "", false, fmt.Errorf("the value is missing")
	}
	if l.s[l.pos] != '"' {
		w := l.token()
		return w, false, nil
	}
	start := l.pos
	l.pos++
	for l.pos < len(l.s) {
		switch l.s[l.pos] {
		case '\\':
			l.pos += 2
			continue
		case '"':
			l.pos++
			var v string
			if err := json.Unmarshal([]byte(l.s[start:l.pos]), &v); err != nil {
				return "", false, fmt.Errorf("the string %s is not valid", l.s[start:l.pos])
			}
			return v, true, nil
		}
		l.pos++
	}
	return "", false, fmt.Errorf("a quotation mark is missing")
}

// parsePath parses an attribute path. A schema prefix of the core User or
// Group schema is removed; attributes of other schemas (extensions) are
// marked ext.
func parsePath(s string) (path, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return path{}, fmt.Errorf("the attribute is missing")
	}
	if strings.HasPrefix(strings.ToLower(s), "urn:") {
		core := false
		for _, schema := range []string{schemaUser, schemaGroup} {
			if len(s) > len(schema)+1 && strings.EqualFold(s[:len(schema)+1], schema+":") {
				s, core = s[len(schema)+1:], true
				break
			}
		}
		if !core {
			return path{attr: strings.ToLower(s), ext: true}, nil
		}
	}
	var p path
	if i := strings.IndexByte(s, '['); i >= 0 {
		end := closingBracket(s, i)
		if end < 0 {
			return path{}, fmt.Errorf("%q: a closing bracket is missing", s)
		}
		f, err := parseFilter(s[i+1 : end])
		if err != nil {
			return path{}, fmt.Errorf("%q: the filter in brackets is not supported", s)
		}
		p.attr, p.filter = strings.ToLower(s[:i]), f
		if rest := s[end+1:]; rest != "" {
			if rest[0] != '.' || len(rest) == 1 {
				return path{}, fmt.Errorf("%q is not a valid attribute path", s)
			}
			p.sub = strings.ToLower(rest[1:])
		}
	} else if attr, sub, ok := strings.Cut(s, "."); ok {
		p.attr, p.sub = strings.ToLower(attr), strings.ToLower(sub)
	} else {
		p.attr = strings.ToLower(s)
	}
	if p.attr == "" || strings.ContainsAny(p.attr+p.sub, " \t\"[]().") {
		return path{}, fmt.Errorf("%q is not a valid attribute path", s)
	}
	return p, nil
}

// closingBracket returns the index of the "]" that closes the "[" at open,
// ignoring brackets in quoted strings, or -1.
func closingBracket(s string, open int) int {
	quoted := false
	for i := open + 1; i < len(s); i++ {
		switch c := s[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && c == ']':
			return i
		}
	}
	return -1
}
