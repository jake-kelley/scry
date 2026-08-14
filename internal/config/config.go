// Package config handles loading, saving, and validating scry's TOML
// configuration file, plus the well-known paths it lives at.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// Root describes a single directory to index.
type Root struct {
	Path          string   `toml:"path"`
	Exclude       []string `toml:"exclude"`        // additional to global
	OfflinePolicy string   `toml:"offline_policy"` // "keep"|"drop", default "keep"
}

// Exclude holds the global exclusion rules applied to every root.
type Exclude struct {
	Names []string `toml:"names"`
	Globs []string `toml:"globs"`
}

// IndexOpts holds indexing behaviour options.
type IndexOpts struct {
	FollowSymlinks bool `toml:"follow_symlinks"`
	Hidden         bool `toml:"hidden"`
}

// HotkeyOpts holds the menu bar app's global search hotkey, per
// "everything-macos-design.md" §7's search window option 3. Combo is a
// "+"-joined string like "alt+space", parsed by internal/hotkey.Parse —
// this package does not depend on internal/hotkey to parse or validate
// it, since a config package growing a platform-UI dependency would be
// backwards; Validate only checks the shape a TOML string can take
// (non-empty), leaving real parsing to the package that owns the key
// vocabulary.
type HotkeyOpts struct {
	Combo string `toml:"combo"`
}

// Config is the top-level scry configuration.
type Config struct {
	Roots   []Root     `toml:"root"`
	Exclude Exclude    `toml:"exclude"`
	Index   IndexOpts  `toml:"index"`
	Hotkey  HotkeyOpts `toml:"hotkey"`
}

// defaultHotkeyCombo mirrors internal/hotkey.DefaultCombo. Duplicated as a
// literal, not imported, per HotkeyOpts's doc comment — config stays free
// of any platform-UI dependency. internal/hotkey/hotkey_test.go's
// TestDefaultComboParses is what keeps the two from drifting apart.
const defaultHotkeyCombo = "alt+space"

// Default returns the built-in default configuration: no roots configured,
// a conservative set of global exclusions, non-intrusive indexing
// options, and Option-Space as the search hotkey.
func Default() Config {
	return Config{
		Roots: nil,
		Exclude: Exclude{
			Names: []string{"node_modules", ".git", ".venv", "__pycache__", "build", "dist"},
			Globs: []string{"*.tmp", "*.o", "*.pyc"},
		},
		Index: IndexOpts{
			FollowSymlinks: false,
			Hidden:         false,
		},
		Hotkey: HotkeyOpts{
			Combo: defaultHotkeyCombo,
		},
	}
}

// ExpandTilde expands a leading "~" or "~/" in p using the current user's
// home directory. Any other use of "~" (e.g. "~user") is unsupported and
// returns an error rather than guessing.
func ExpandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		if strings.HasPrefix(p, "~") {
			return "", fmt.Errorf("config: unsupported home-directory form %q (only \"~\" or \"~/...\" is supported)", p)
		}
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: expand tilde: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// ConfigPath returns the path to scry's config file:
// ~/.config/scry/config.toml, unless running on darwin where ~/.config
// does not exist but ~/Library/Application Support does, in which case
// ~/Library/Application Support/scry/config.toml is used.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: config path: %w", err)
	}
	dotConfig := filepath.Join(home, ".config")
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(dotConfig); os.IsNotExist(err) {
			appSupport := filepath.Join(home, "Library", "Application Support")
			if _, err := os.Stat(appSupport); err == nil {
				return filepath.Join(appSupport, "scry", "config.toml"), nil
			}
		}
	}
	return filepath.Join(dotConfig, "scry", "config.toml"), nil
}

// CacheDir returns scry's cache directory: ~/.cache/scry, or
// ~/Library/Caches/scry on darwin. The directory is created with mode
// 0700 if it does not already exist.
func CacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cache dir: %w", err)
	}
	var dir string
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Caches", "scry")
	} else {
		dir = filepath.Join(home, ".cache", "scry")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("config: cache dir: %w", err)
	}
	return dir, nil
}

// Load reads and parses the config file at path. If the file does not
// exist, Load returns Default() with a nil error — a first run is not an
// error condition. A malformed file is an error and is never silently
// replaced by defaults, since that could wipe a user's root list.
//
// Fields left unset in the file are filled in from Default(); in
// particular an empty or absent [exclude] block does not mean "exclude
// nothing".
func Load(path string) (Config, error) {
	def := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return Config{}, fmt.Errorf("config: load %s: %w", path, err)
	}

	var raw struct {
		Roots   []Root      `toml:"root"`
		Exclude *Exclude    `toml:"exclude"`
		Index   *IndexOpts  `toml:"index"`
		Hotkey  *HotkeyOpts `toml:"hotkey"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}

	c := def
	c.Roots = raw.Roots
	if raw.Exclude != nil {
		c.Exclude = *raw.Exclude
	}
	if raw.Index != nil {
		c.Index = *raw.Index
	}
	if raw.Hotkey != nil {
		c.Hotkey = *raw.Hotkey
	}
	normalizeOfflinePolicies(c.Roots)
	normalizeHotkey(&c.Hotkey)
	return c, nil
}

// normalizeHotkey fills in defaultHotkeyCombo when combo was left unset —
// an explicit but empty `[hotkey]` block (or one missing the `combo` key)
// means "use the default", the same way an old config with no
// offline_policy field means "keep" rather than "invalid". Mirrors
// normalizeOfflinePolicies's role for HotkeyOpts.
func normalizeHotkey(h *HotkeyOpts) {
	if strings.TrimSpace(h.Combo) == "" {
		h.Combo = defaultHotkeyCombo
	}
}

// normalizeOfflinePolicies fills in the default offline_policy ("keep") on
// any root that left it unset. Validate treats "" as valid so an old config
// file without the field still loads without error; this is what turns
// that "" into the real default in memory, in place, once — on Load (an
// existing file with the field omitted) and on Save (so a newly written
// file never round-trips an empty string). A root with an explicit value
// ("keep" or "drop") is left untouched.
func normalizeOfflinePolicies(roots []Root) {
	for i := range roots {
		if roots[i].OfflinePolicy == "" {
			roots[i].OfflinePolicy = "keep"
		}
	}
}

// Save writes c to path atomically: a temp file is written in the same
// directory, fsynced, and renamed into place. Parent directories are
// created as needed. The final file has mode 0600.
func Save(path string, c Config) error {
	// Copy Roots before normalizing so the caller's slice/config value is
	// never mutated as a side effect of saving.
	roots := append([]Root(nil), c.Roots...)
	normalizeOfflinePolicies(roots)
	c.Roots = roots
	normalizeHotkey(&c.Hotkey)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Ensure the temp file is cleaned up on any failure path.
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}

	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: save %s: %w", path, err)
	}
	success = true
	return nil
}

// Validate checks c for problems and returns all of them (not just the
// first): empty root paths, duplicate root paths (after ExpandTilde,
// compared case-insensitively on windows/darwin), an offline_policy not
// in {"keep", "drop", ""}, and exclude globs that fail filepath.Match's
// syntax check.
func (c Config) Validate() []error {
	var errs []error

	seen := make(map[string]string) // normalized -> original
	for _, r := range c.Roots {
		if strings.TrimSpace(r.Path) == "" {
			errs = append(errs, errors.New("config: root path is empty"))
		} else {
			norm, err := ExpandTilde(r.Path)
			if err != nil {
				errs = append(errs, fmt.Errorf("config: root %q: %w", r.Path, err))
			} else {
				norm = filepath.Clean(norm)
				key := norm
				if caseInsensitiveFS() {
					key = strings.ToLower(norm)
				}
				if orig, ok := seen[key]; ok {
					errs = append(errs, fmt.Errorf("config: duplicate root %q (also given as %q)", r.Path, orig))
				} else {
					seen[key] = r.Path
				}
			}
		}

		switch r.OfflinePolicy {
		case "keep", "drop", "":
		default:
			errs = append(errs, fmt.Errorf("config: root %q: invalid offline_policy %q (must be \"keep\" or \"drop\")", r.Path, r.OfflinePolicy))
		}

		for _, g := range r.Exclude {
			if _, err := filepath.Match(g, ""); err != nil {
				errs = append(errs, fmt.Errorf("config: root %q: invalid exclude glob %q: %w", r.Path, g, err))
			}
		}
	}

	for _, g := range c.Exclude.Globs {
		if _, err := filepath.Match(g, ""); err != nil {
			errs = append(errs, fmt.Errorf("config: invalid exclude glob %q: %w", g, err))
		}
	}

	return errs
}

// AddRoot adds path as a new root, after expanding a leading tilde and
// cleaning it. It is an error if the root is already present (compared
// case-insensitively on windows/darwin).
func (c *Config) AddRoot(path string) error {
	norm, err := ExpandTilde(path)
	if err != nil {
		return err
	}
	norm = filepath.Clean(norm)
	key := norm
	if caseInsensitiveFS() {
		key = strings.ToLower(norm)
	}
	for _, r := range c.Roots {
		existing, err := ExpandTilde(r.Path)
		if err != nil {
			continue
		}
		existing = filepath.Clean(existing)
		if caseInsensitiveFS() {
			existing = strings.ToLower(existing)
		}
		if existing == key {
			return fmt.Errorf("config: root %q is already configured", path)
		}
	}
	c.Roots = append(c.Roots, Root{Path: norm})
	return nil
}

// RemoveRoot removes the root matching path (same matching rules as
// AddRoot) and reports whether a root was actually removed.
func (c *Config) RemoveRoot(path string) (bool, error) {
	norm, err := ExpandTilde(path)
	if err != nil {
		return false, err
	}
	norm = filepath.Clean(norm)
	key := norm
	if caseInsensitiveFS() {
		key = strings.ToLower(norm)
	}
	for i, r := range c.Roots {
		existing, err := ExpandTilde(r.Path)
		if err != nil {
			continue
		}
		existing = filepath.Clean(existing)
		if caseInsensitiveFS() {
			existing = strings.ToLower(existing)
		}
		if existing == key {
			c.Roots = append(c.Roots[:i], c.Roots[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// caseInsensitiveFS reports whether root-path comparisons should be
// case-insensitive on the current platform (windows and darwin both
// default to case-insensitive filesystems).
func caseInsensitiveFS() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}
