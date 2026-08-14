package qsyntax

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseForms(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Term
	}{
		{"fuzzy", "rprt24", []Term{{Kind: KindFuzzy, Value: "rprt24"}}},
		{"literal", `"foo bar"`, []Term{{Kind: KindLiteral, Value: "foo bar"}}},
		{"literal with escaped quote", `"say \"hi\""`, []Term{{Kind: KindLiteral, Value: `say "hi"`}}},
		{"glob star", "*.go", []Term{{Kind: KindGlob, Value: "*.go"}}},
		{"glob question", "rep?rt", []Term{{Kind: KindGlob, Value: "rep?rt"}}},
		{"ext single", "ext:go", []Term{{Kind: KindExt, Value: "go"}}},
		{"ext multi", "ext:go,rs", []Term{{Kind: KindExt, Value: "go,rs"}}},
		{"ext leading dot", "ext:.go,.rs", []Term{{Kind: KindExt, Value: "go,rs"}}},
		{"path", "path:src", []Term{{Kind: KindPath, Value: "src"}}},
		{"root", "root:code", []Term{{Kind: KindRoot, Value: "code"}}},
		{"negated fuzzy", "!vendor", []Term{{Kind: KindFuzzy, Value: "vendor", Negated: true}}},
		{"negated literal", `!"foo bar"`, []Term{{Kind: KindLiteral, Value: "foo bar", Negated: true}}},
		{"negated glob", "!*.tmp", []Term{{Kind: KindGlob, Value: "*.tmp", Negated: true}}},
		{"negated ext", "!ext:go", []Term{{Kind: KindExt, Value: "go", Negated: true}}},
		{"negated path", "!path:src", []Term{{Kind: KindPath, Value: "src", Negated: true}}},
		{"negated root", "!root:code", []Term{{Kind: KindRoot, Value: "code", Negated: true}}},
		{"empty query", "", nil},
		{"whitespace only query", "   ", nil},
		{
			"combination",
			"ext:go !vendor path:src rprt",
			[]Term{
				{Kind: KindExt, Value: "go"},
				{Kind: KindFuzzy, Value: "vendor", Negated: true},
				{Kind: KindPath, Value: "src"},
				{Kind: KindFuzzy, Value: "rprt"},
			},
		},
		{
			"case insensitive",
			"EXT:GO ROOT:Code",
			[]Term{
				{Kind: KindExt, Value: "go"},
				{Kind: KindRoot, Value: "code"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tt.in, err)
			}
			if q.Raw != tt.in {
				t.Errorf("Raw = %q, want %q", q.Raw, tt.in)
			}
			if len(q.Terms) != len(tt.want) {
				t.Fatalf("Terms = %+v, want %+v", q.Terms, tt.want)
			}
			for i, term := range q.Terms {
				if term != tt.want[i] {
					t.Errorf("Terms[%d] = %+v, want %+v", i, term, tt.want[i])
				}
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"unterminated quote", `"foo bar`},
		{"empty ext list", "ext:"},
		{"ext with trailing comma", "ext:go,"},
		{"ext with only comma", "ext:,"},
		{"empty path value", "path:"},
		{"empty root value", "root:"},
		{"bare negation", "!"},
		{"bare negation among terms", "rprt ! path:src"},
		{"negation before whitespace", "! rprt"},
		{"malformed glob", "rep[a*rt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			if err == nil {
				t.Fatalf("Parse(%q) returned no error, want one", tt.in)
			}
		})
	}
}

func TestMatchNameExt(t *testing.T) {
	q, err := Parse("ext:go,rs")
	if err != nil {
		t.Fatal(err)
	}
	if !q.MatchName("main.go") {
		t.Error("main.go should match ext:go,rs")
	}
	if !q.MatchName("lib.rs") {
		t.Error("lib.rs should match ext:go,rs")
	}
	if q.MatchName("main.py") {
		t.Error("main.py should not match ext:go,rs")
	}

	qDot, err := Parse("ext:.go")
	if err != nil {
		t.Fatal(err)
	}
	if !qDot.MatchName("main.go") {
		t.Error("main.go should match ext:.go (leading dot optional)")
	}
}

func TestMatchPathMiddleComponent(t *testing.T) {
	q, err := Parse("path:src")
	if err != nil {
		t.Fatal(err)
	}
	if !q.MatchPath(`C:\project\src\main.go`) {
		t.Error("path with src as a middle component should match path:src")
	}
	if q.MatchPath(`C:\project\other\main.go`) {
		t.Error("path without src should not match path:src")
	}
}

func TestMatchGlobBaseNameOnly(t *testing.T) {
	q, err := Parse("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !q.MatchName("main.go") {
		t.Error("main.go should match *.go")
	}
	// The glob must not match against a path — only the base name.
	//
	// The separator has to come from the OS rather than being written as a
	// literal backslash: on darwin a backslash is filepath.Match's escape
	// character, not a separator, so `src\main.go` is one unremarkable base
	// name there and the assertion means nothing.
	if q.MatchName("src" + string(filepath.Separator) + "main.go") {
		t.Error("*.go should not match a base name that itself embeds a separator")
	}
}

func TestMatchLiteralWithSpace(t *testing.T) {
	q, err := Parse(`"foo bar"`)
	if err != nil {
		t.Fatal(err)
	}
	if !q.MatchName("a foo bar b") {
		t.Error(`"foo bar" should match a name containing "foo bar"`)
	}
	if q.MatchName("foobar") {
		t.Error(`"foo bar" should not match "foobar" (no space)`)
	}
}

func TestNegationInverts(t *testing.T) {
	q, err := Parse("!ext:go")
	if err != nil {
		t.Fatal(err)
	}
	if q.MatchName("main.go") {
		t.Error("main.go should not match !ext:go")
	}
	if !q.MatchName("main.py") {
		t.Error("main.py should match !ext:go")
	}

	qFuzzy, err := Parse("!vendor")
	if err != nil {
		t.Fatal(err)
	}
	if qFuzzy.MatchName("vendor.go") {
		t.Error("vendor.go should not match !vendor")
	}
	if !qFuzzy.MatchName("main.go") {
		t.Error("main.go should match !vendor")
	}
}

func TestEmptyQueryMatchesEverything(t *testing.T) {
	q, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if !q.MatchName("anything.go") {
		t.Error("empty query should match any name")
	}
	if !q.MatchPath(`C:\anything\at\all.go`) {
		t.Error("empty query should match any path")
	}
	if terms := q.FuzzyTerms(); len(terms) != 0 {
		t.Errorf("FuzzyTerms() = %v, want empty", terms)
	}
	if _, ok := q.RootFilter(); ok {
		t.Error("RootFilter() should report no filter for an empty query")
	}
}

func TestFuzzyTerms(t *testing.T) {
	q, err := Parse("rprt24 ext:go !vendor another")
	if err != nil {
		t.Fatal(err)
	}
	got := q.FuzzyTerms()
	want := []string{"rprt24", "another"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("FuzzyTerms() = %v, want %v", got, want)
	}
}

func TestRootFilter(t *testing.T) {
	q, err := Parse("root:code rprt")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := q.RootFilter()
	if !ok || v != "code" {
		t.Errorf("RootFilter() = (%q, %v), want (\"code\", true)", v, ok)
	}

	qNone, err := Parse("rprt")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := qNone.RootFilter(); ok {
		t.Error("RootFilter() should report false when there is no root: term")
	}
}

func TestCombinationQuery(t *testing.T) {
	q, err := Parse("ext:go !vendor path:src rprt")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if !q.MatchName("report.go") {
		t.Error("report.go should match ext:go !vendor rprt")
	}
	if q.MatchName("vendorreport.go") {
		t.Error("vendorreport.go should be excluded by !vendor")
	}
	if !q.MatchPath(`C:\proj\src\report.go`) {
		t.Error("path under src should match path:src")
	}
	if q.MatchPath(`C:\proj\other\report.go`) {
		t.Error("path not under src should not match path:src")
	}
	if got := q.FuzzyTerms(); len(got) != 1 || got[0] != "rprt" {
		t.Errorf("FuzzyTerms() = %v, want [rprt]", got)
	}
}
