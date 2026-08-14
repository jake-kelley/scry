package roots

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// withFoldCase runs fn with FoldCase temporarily set to v, restoring the
// original value afterward.
func withFoldCase(t *testing.T, v bool, fn func()) {
	t.Helper()
	orig := FoldCase
	FoldCase = v
	defer func() { FoldCase = orig }()
	fn()
}

func TestContainsComponentBoundary(t *testing.T) {
	cases := []struct {
		name          string
		parent, child string
		want          bool
	}{
		{"sibling with shared prefix is not contained", "/a/b", "/a/bc", false},
		{"direct child is contained", "/a/b", "/a/b/c", true},
		{"identical paths are contained", "/a/b", "/a/b", true},
		{"unrelated paths", "/a/b", "/x/y", false},
		{"parent longer than child", "/a/b/c", "/a/b", false},
		{"trailing separator on parent doesn't matter", "/a/b/", "/a/b/c", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFoldCase(t, false, func() {
				got := Contains(filepath.FromSlash(tc.parent), filepath.FromSlash(tc.child))
				if got != tc.want {
					t.Errorf("Contains(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
				}
			})
		})
	}
}

func TestContainsCaseFolding(t *testing.T) {
	withFoldCase(t, true, func() {
		if !Contains(filepath.FromSlash("/a/B"), filepath.FromSlash("/A/b/c")) {
			t.Errorf("expected case-folded containment to match")
		}
	})
	withFoldCase(t, false, func() {
		if Contains(filepath.FromSlash("/a/B"), filepath.FromSlash("/A/b/c")) {
			t.Errorf("expected case-sensitive comparison to reject mismatched case")
		}
	})
}

// findWarning returns the reason attached to path, or "" if none.
func findWarning(warnings []Warning, path string) string {
	for _, w := range warnings {
		if w.Path == path {
			return w.Reason
		}
	}
	return ""
}

func hasKept(kept []string, path string) bool {
	for _, k := range kept {
		if k == path {
			return true
		}
	}
	return false
}

func TestNormalizeThreeDeepNestingCollapses(t *testing.T) {
	dir := t.TempDir()
	a := dir
	b := filepath.Join(dir, "b")
	c := filepath.Join(dir, "b", "c")
	if err := os.MkdirAll(c, 0o755); err != nil {
		t.Fatal(err)
	}

	kept, warnings := Normalize([]string{c, a, b})

	if len(kept) != 1 {
		t.Fatalf("kept = %v, want exactly one root", kept)
	}
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	if kept[0] != resolvedA {
		t.Errorf("kept[0] = %q, want %q", kept[0], resolvedA)
	}

	resolvedB, _ := filepath.EvalSymlinks(b)
	resolvedC, _ := filepath.EvalSymlinks(c)
	if reason := findWarning(warnings, resolvedB); reason == "" {
		t.Errorf("expected a warning for absorbed child %q, got none; warnings=%v", resolvedB, warnings)
	}
	if reason := findWarning(warnings, resolvedC); reason == "" {
		t.Errorf("expected a warning for absorbed grandchild %q, got none; warnings=%v", resolvedC, warnings)
	}
}

func TestNormalizeIdenticalPathsDeduped(t *testing.T) {
	dir := t.TempDir()
	kept, warnings := Normalize([]string{dir, dir, dir})
	if len(kept) != 1 {
		t.Fatalf("kept = %v, want exactly one root", kept)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2 duplicate warnings", warnings)
	}
}

func TestNormalizeChildOfExistingRootIsNoop(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	kept, _ := Normalize([]string{dir, child})
	if len(kept) != 1 {
		t.Fatalf("kept = %v, want exactly one root", kept)
	}
}

func TestNormalizeNonexistentPathSurvivesWithWarning(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")

	kept, warnings := Normalize([]string{missing})

	if len(kept) != 1 {
		t.Fatalf("kept = %v, want the missing root to survive", kept)
	}
	abs, err := filepath.Abs(filepath.Clean(missing))
	if err != nil {
		t.Fatal(err)
	}
	if kept[0] != abs {
		t.Errorf("kept[0] = %q, want %q", kept[0], abs)
	}
	if reason := findWarning(warnings, abs); reason != "path does not exist" {
		t.Errorf("warning reason = %q, want %q", reason, "path does not exist")
	}
}

func TestNormalizeFileNotDirectoryIsDropped(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	kept, warnings := Normalize([]string{file})

	if len(kept) != 0 {
		t.Fatalf("kept = %v, want the file to be dropped", kept)
	}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	if reason := findWarning(warnings, resolved); reason != "not a directory" {
		t.Errorf("warning reason = %q, want %q", reason, "not a directory")
	}
}

// TestNormalizeCaseFoldedDuplicatesSynthetic exercises FoldCase-driven
// dedup at the string level rather than relying on the host filesystem's
// actual case sensitivity, so it behaves the same on every OS: on a
// case-insensitive filesystem (Windows, default APFS) EvalSymlinks would
// itself collapse both variants to the on-disk case before dedup ever runs,
// which would test the filesystem instead of this package's FoldCase logic.
func TestNormalizeCaseFoldedDuplicatesSynthetic(t *testing.T) {
	// Exercises the same behaviour without touching the filesystem, so it
	// runs identically regardless of host OS or filesystem case-sensitivity.
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	lowerPath := resolved
	upperPath := swapCase(resolved)
	if lowerPath == upperPath {
		t.Skip("path has no letters to case-swap")
	}

	withFoldCase(t, true, func() {
		kept, _ := Normalize([]string{lowerPath, upperPath})
		if len(kept) != 1 {
			t.Errorf("FoldCase=true: kept = %v, want exactly one root (case-insensitive filesystem stats both to the same entry)", kept)
		}
	})
}

// swapCase flips the case of every letter in s.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

func TestNormalizeSymlinkAbsorbedByTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation not permitted on this OS/config: %v", err)
	}

	kept, _ := Normalize([]string{target, link})
	if len(kept) != 1 {
		t.Fatalf("kept = %v, want the symlink to resolve to the same root as its target", kept)
	}
}

func TestNormalizeUnrelatedRootsAllSurvive(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	kept, warnings := Normalize([]string{dirA, dirB})

	sort.Strings(kept)
	if len(kept) != 2 {
		t.Fatalf("kept = %v, want both unrelated roots to survive", kept)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	resolvedA, _ := filepath.EvalSymlinks(dirA)
	resolvedB, _ := filepath.EvalSymlinks(dirB)
	if !hasKept(kept, resolvedA) || !hasKept(kept, resolvedB) {
		t.Errorf("kept = %v, want %q and %q", kept, resolvedA, resolvedB)
	}
}

func TestNormalizeStableSortedOutput(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	kept1, _ := Normalize([]string{dirA, dirB})
	kept2, _ := Normalize([]string{dirB, dirA})

	if len(kept1) != len(kept2) {
		t.Fatalf("kept1=%v kept2=%v differ in length", kept1, kept2)
	}
	for i := range kept1 {
		if kept1[i] != kept2[i] {
			t.Errorf("order not deterministic: kept1=%v kept2=%v", kept1, kept2)
		}
	}
	if !sort.StringsAreSorted(kept1) {
		t.Errorf("kept1 = %v, not sorted", kept1)
	}
}

func TestNormalizeEmptyInput(t *testing.T) {
	kept, warnings := Normalize(nil)
	if len(kept) != 0 || len(warnings) != 0 {
		t.Errorf("kept=%v warnings=%v, want both empty", kept, warnings)
	}
}
