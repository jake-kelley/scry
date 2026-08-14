// Package qsyntax parses scry's query syntax into a deterministic Query.
//
// A query string is whitespace-separated terms, each one of:
//
//	rprt24            fuzzy term (the default when nothing else matches)
//	"foo bar"         literal substring, no fuzzing, spaces preserved
//	*.go   rep?rt     glob against the base name (filepath.Match)
//	ext:go,rs         extension filter, comma-separated, leading dot optional
//	path:src          substring must appear in the full path, not just the base name
//	root:code         restrict results to roots whose path contains this substring
//	!vendor           negation — prefix any form with ! to invert it
//
// Matching is split in two, deliberately: this package answers only the
// deterministic filters (literal, glob, ext, path, root). Fuzzy ranking of
// the bare fuzzy terms is the caller's job, via FuzzyTerms() feeding
// internal/fuzzy — Query never scores anything itself. The one exception is
// a *negated* fuzzy term (e.g. "!vendor"): since there is no way to express
// "rank low" as a boolean AND filter, a negated fuzzy term is instead
// evaluated deterministically as a substring exclusion on the base name.
package qsyntax

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Kind identifies the syntactic form of a Term.
type Kind int

const (
	// KindFuzzy is a bare term with no special syntax, fuzzy-matched by the
	// caller (unless negated — see the package doc).
	KindFuzzy Kind = iota
	// KindLiteral is a quoted term, matched as a literal substring.
	KindLiteral
	// KindGlob is a term containing an unescaped * or ?, matched against
	// the base name with filepath.Match.
	KindGlob
	// KindExt is an ext: term, matched against the file extension.
	KindExt
	// KindPath is a path: term, matched as a substring of the full path.
	KindPath
	// KindRoot is a root: term, restricting which roots are searched.
	KindRoot
)

// String returns a human-readable name for k, mainly for error messages and
// tests.
func (k Kind) String() string {
	switch k {
	case KindFuzzy:
		return "fuzzy"
	case KindLiteral:
		return "literal"
	case KindGlob:
		return "glob"
	case KindExt:
		return "ext"
	case KindPath:
		return "path"
	case KindRoot:
		return "root"
	default:
		return "unknown"
	}
}

// Term is a single parsed piece of a query. Value is already lowercased and
// stripped of its syntax markers (quotes, "ext:", etc.) at Parse time.
type Term struct {
	Kind    Kind
	Value   string
	Negated bool
}

// Query is a parsed sequence of terms, all ANDed together. An empty Query
// (no terms) matches everything.
type Query struct {
	Terms []Term
	Raw   string // the original, unmodified input passed to Parse
}

// rawToken is an intermediate result of tokenizing, before a term's syntax
// (ext:, path:, glob chars, ...) has been classified.
type rawToken struct {
	value   string
	quoted  bool
	negated bool
}

// Parse parses a query string into a Query. It lowercases the input once
// (matching is case-insensitive throughout) and returns an error naming the
// offending term for any malformed input — a parse error is never silently
// swallowed into a dropped filter.
func Parse(s string) (Query, error) {
	lower := strings.ToLower(s)

	toks, err := tokenize(lower)
	if err != nil {
		return Query{}, err
	}

	terms := make([]Term, 0, len(toks))
	for _, tok := range toks {
		t, err := classify(tok)
		if err != nil {
			return Query{}, err
		}
		terms = append(terms, t)
	}

	return Query{Terms: terms, Raw: s}, nil
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

// tokenize splits s into raw tokens, honoring quotes (with \" escaping) and
// leading "!" negation markers.
func tokenize(s string) ([]rawToken, error) {
	runes := []rune(s)
	n := len(runes)
	var toks []rawToken

	i := 0
	for i < n {
		for i < n && isSpace(runes[i]) {
			i++
		}
		if i >= n {
			break
		}

		negated := false
		if runes[i] == '!' {
			negated = true
			i++
		}
		if i >= n || isSpace(runes[i]) {
			return nil, fmt.Errorf("qsyntax: bare \"!\" is not a valid term")
		}

		if runes[i] == '"' {
			i++ // consume opening quote
			var sb strings.Builder
			closed := false
			for i < n {
				c := runes[i]
				if c == '\\' && i+1 < n && runes[i+1] == '"' {
					sb.WriteRune('"')
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				sb.WriteRune(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("qsyntax: unterminated quote in term: %q", "\""+sb.String())
			}
			toks = append(toks, rawToken{value: sb.String(), quoted: true, negated: negated})
			continue
		}

		start := i
		for i < n && !isSpace(runes[i]) {
			i++
		}
		toks = append(toks, rawToken{value: string(runes[start:i]), quoted: false, negated: negated})
	}

	return toks, nil
}

// classify turns a raw token into a Term, validating its syntax.
func classify(tok rawToken) (Term, error) {
	if tok.quoted {
		return Term{Kind: KindLiteral, Value: tok.value, Negated: tok.negated}, nil
	}

	v := tok.value
	if v == "" {
		return Term{}, fmt.Errorf("qsyntax: empty term")
	}

	switch {
	case strings.HasPrefix(v, "ext:"):
		rest := v[len("ext:"):]
		if rest == "" {
			return Term{}, fmt.Errorf("qsyntax: %q has no extensions", v)
		}
		parts := strings.Split(rest, ",")
		exts := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimPrefix(p, ".")
			if p == "" {
				return Term{}, fmt.Errorf("qsyntax: %q has an empty extension", v)
			}
			exts = append(exts, p)
		}
		return Term{Kind: KindExt, Value: strings.Join(exts, ","), Negated: tok.negated}, nil

	case strings.HasPrefix(v, "path:"):
		rest := v[len("path:"):]
		if rest == "" {
			return Term{}, fmt.Errorf("qsyntax: %q has no value", v)
		}
		return Term{Kind: KindPath, Value: toSlash(rest), Negated: tok.negated}, nil

	case strings.HasPrefix(v, "root:"):
		rest := v[len("root:"):]
		if rest == "" {
			return Term{}, fmt.Errorf("qsyntax: %q has no value", v)
		}
		return Term{Kind: KindRoot, Value: toSlash(rest), Negated: tok.negated}, nil

	case strings.ContainsAny(v, "*?"):
		if _, err := filepath.Match(v, ""); err != nil {
			return Term{}, fmt.Errorf("qsyntax: malformed glob %q: %w", v, err)
		}
		return Term{Kind: KindGlob, Value: v, Negated: tok.negated}, nil

	default:
		return Term{Kind: KindFuzzy, Value: v, Negated: tok.negated}, nil
	}
}

// toSlash normalizes backslashes to forward slashes, so path:/root: values
// and the paths they're compared against match regardless of which
// separator style either side used.
func toSlash(s string) string {
	return strings.ReplaceAll(s, "\\", "/")
}

// MatchName reports whether name (a base file name, not a full path)
// satisfies every deterministic term that applies to base names: literal,
// glob, and ext terms, plus negated fuzzy terms (see the package doc).
// Positive fuzzy terms, path terms, and root terms are not evaluated here.
func (q Query) MatchName(name string) bool {
	lname := strings.ToLower(name)

	for _, t := range q.Terms {
		var ok bool
		switch t.Kind {
		case KindLiteral:
			ok = strings.Contains(lname, t.Value)
		case KindGlob:
			m, _ := filepath.Match(t.Value, lname)
			ok = m
		case KindExt:
			ok = extMatches(lname, t.Value)
		case KindFuzzy:
			if !t.Negated {
				// Positive fuzzy terms are the caller's job via
				// FuzzyTerms(); they never fail a deterministic match.
				continue
			}
			ok = strings.Contains(lname, t.Value)
		default:
			continue
		}
		if ok == t.Negated {
			return false
		}
	}
	return true
}

// MatchPath reports whether path (a full path) satisfies every path: term.
// It does not re-check literal/glob/ext/fuzzy terms — call MatchName with
// the path's base name for those.
func (q Query) MatchPath(path string) bool {
	lpath := toSlash(strings.ToLower(path))

	for _, t := range q.Terms {
		if t.Kind != KindPath {
			continue
		}
		ok := strings.Contains(lpath, t.Value)
		if ok == t.Negated {
			return false
		}
	}
	return true
}

// extMatches reports whether name's extension (without the leading dot) is
// one of the comma-separated extensions in list. Both name and list are
// assumed already lowercased.
func extMatches(name, list string) bool {
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	if ext == "" {
		return false
	}
	for _, e := range strings.Split(list, ",") {
		if e == ext {
			return true
		}
	}
	return false
}

// FuzzyTerms returns the bare, non-negated fuzzy terms for the caller to
// feed to internal/fuzzy. Query itself never scores or ranks these — see
// the package doc for the deterministic/fuzzy split.
func (q Query) FuzzyTerms() []string {
	var out []string
	for _, t := range q.Terms {
		if t.Kind == KindFuzzy && !t.Negated {
			out = append(out, t.Value)
		}
	}
	return out
}

// RootFilter returns the substring a root's path must contain, and true, if
// the query has a positive (non-negated) root: term. If none is present it
// returns ("", false). Only the first positive root: term is exposed;
// negated root: terms are parsed and stored in Terms but can't be expressed
// through this two-value contract, so they have no effect on RootFilter.
func (q Query) RootFilter() (string, bool) {
	for _, t := range q.Terms {
		if t.Kind == KindRoot && !t.Negated {
			return t.Value, true
		}
	}
	return "", false
}
