package qsyntax

import "testing"

// FuzzParse asserts that Parse never panics on arbitrary input, whether it
// succeeds or returns an error. If it does return a Query, exercising
// MatchName/MatchPath/FuzzyTerms/RootFilter on it must not panic either.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"rprt24",
		`"foo bar"`,
		`"unterminated`,
		`"escaped \" quote"`,
		"*.go",
		"rep?rt",
		"ext:go,rs",
		"ext:",
		"ext:,",
		"path:src",
		"path:",
		"root:code",
		"root:",
		"!vendor",
		"!",
		"! rprt",
		"ext:go !vendor path:src rprt",
		"rep[a*rt",
		"\\",
		"****",
		"!!!!",
		"\"\"",
		"ext:go,rs,,",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		q, err := Parse(s)
		if err != nil {
			return
		}
		_ = q.MatchName("some/name.go")
		_ = q.MatchPath(`C:\some\path\name.go`)
		_ = q.FuzzyTerms()
		_, _ = q.RootFilter()
	})
}
