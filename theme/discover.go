package theme

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Info identifies a discovered theme without loading it.
type Info struct {
	Name string
	// Path is the file the theme came from, empty for a built-in.
	Path string
}

// Diagnostic reports a theme that could not be used.
type Diagnostic struct {
	// Path is the file at fault, empty when the problem is not file-bound.
	Path string
	// Message says what is wrong, in a form fit to show the user.
	Message string
}

// Options selects where themes are discovered from.
type Options struct {
	// Dir is the user's themes directory; every *.json in it is a candidate.
	// Usually config.ThemesDir().
	Dir string
	// Paths are extra theme files and directories named explicitly, from the
	// "themes" setting. A directory contributes its *.json entries.
	Paths []string
	// Extra are themes supplied programmatically by an embedder or extension.
	Extra []*Theme
	// SkipBuiltins leaves the compiled-in themes out. Only useful in tests.
	SkipBuiltins bool
}

// Set is the result of discovery: every usable theme, and a diagnostic for
// every candidate that was not.
type Set struct {
	themes []*Theme
	diags  []Diagnostic
}

// Discover collects themes from the built-ins, the themes directory, the
// explicitly named paths, and the programmatically supplied ones — in that
// order. The first theme to claim a name keeps it, so a built-in cannot be
// shadowed; the result is sorted by name.
func Discover(opts Options) *Set {
	s := &Set{}
	seen := make(map[string]bool)
	add := func(t *Theme) {
		if seen[t.Name] {
			return
		}
		seen[t.Name] = true
		s.themes = append(s.themes, t)
	}

	if !opts.SkipBuiltins {
		all, err := builtins()
		if err != nil {
			s.diags = append(s.diags, Diagnostic{Message: err.Error()})
		}
		for _, name := range BuiltinNames {
			if t, ok := all[name]; ok {
				add(t)
			}
		}
	}

	for _, path := range dirEntries(opts.Dir) {
		s.addFile(path, add)
	}

	for _, p := range opts.Paths {
		info, err := os.Stat(p)
		if err != nil {
			s.diags = append(s.diags, Diagnostic{Path: p, Message: "theme not found: " + err.Error()})
			continue
		}
		if info.IsDir() {
			for _, path := range dirEntries(p) {
				s.addFile(path, add)
			}
			continue
		}
		s.addFile(p, add)
	}

	for _, t := range opts.Extra {
		if t != nil {
			add(t)
		}
	}

	sort.SliceStable(s.themes, func(i, j int) bool { return s.themes[i].Name < s.themes[j].Name })
	return s
}

func (s *Set) addFile(path string, add func(*Theme)) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.diags = append(s.diags, Diagnostic{Path: path, Message: "cannot read theme: " + err.Error()})
		return
	}
	t, err := Parse(data, path)
	if err != nil {
		s.diags = append(s.diags, Diagnostic{Path: path, Message: err.Error()})
		return
	}
	add(t)
}

// dirEntries returns the *.json files directly inside dir, sorted, or nothing
// when dir is unset or unreadable.
func dirEntries(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	return paths
}

// Themes returns every discovered theme, sorted by name.
func (s *Set) Themes() []*Theme { return append([]*Theme(nil), s.themes...) }

// Names returns the discovered theme names, sorted.
func (s *Set) Names() []string {
	names := make([]string, 0, len(s.themes))
	for _, t := range s.themes {
		names = append(names, t.Name)
	}
	return names
}

// Infos returns name and origin for every discovered theme, sorted by name.
func (s *Set) Infos() []Info {
	infos := make([]Info, 0, len(s.themes))
	for _, t := range s.themes {
		infos = append(infos, Info{Name: t.Name, Path: t.Path})
	}
	return infos
}

// Diagnostics returns one entry per candidate that could not be loaded.
func (s *Set) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), s.diags...) }

// Get returns a theme by name.
func (s *Set) Get(name string) (*Theme, bool) {
	for _, t := range s.themes {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}

// Mode is a terminal's background: light or dark.
type Mode string

const (
	Dark  Mode = "dark"
	Light Mode = "light"
)

// AutoSetting is a theme setting of the form "light/dark", naming one theme for
// each terminal background.
type AutoSetting struct {
	Light string
	Dark  string
}

// ParseAutoSetting reads the automatic "<light>/<dark>" form of the theme
// setting. It returns false for a plain theme name, for an empty setting, and
// for anything with more than one "/" or an empty side.
func ParseAutoSetting(setting string) (AutoSetting, bool) {
	if setting == "" {
		return AutoSetting{}, false
	}
	slash := strings.Index(setting, "/")
	if slash == -1 || strings.Contains(setting[slash+1:], "/") {
		return AutoSetting{}, false
	}
	light := strings.TrimSpace(setting[:slash])
	dark := strings.TrimSpace(setting[slash+1:])
	if light == "" || dark == "" {
		return AutoSetting{}, false
	}
	return AutoSetting{Light: light, Dark: dark}, true
}

// ResolveSetting turns a theme setting into the theme name to load, given the
// terminal's background. A setting that still contains a "/" after failing to
// parse as the automatic form names nothing, since a theme cannot carry one.
func ResolveSetting(setting string, background Mode) string {
	if auto, ok := ParseAutoSetting(setting); ok {
		if background == Light {
			return auto.Light
		}
		return auto.Dark
	}
	if strings.Contains(setting, "/") {
		return ""
	}
	return setting
}

// Resolve picks the theme the setting asks for, falling back to the built-in
// matching the terminal background when the setting is empty or names a theme
// that is not there. The second result is false when a named theme was missing,
// so the caller can say so.
func (s *Set) Resolve(setting string, background Mode) (*Theme, bool) {
	name := ResolveSetting(setting, background)
	if name != "" {
		if t, ok := s.Get(name); ok {
			return t, true
		}
	}
	fallback := string(background)
	if t, ok := s.Get(fallback); ok {
		return t, name == ""
	}
	if len(s.themes) > 0 {
		return s.themes[0], name == ""
	}
	return nil, false
}

// Detection records how a terminal's background was determined.
type Detection struct {
	Mode Mode
	// Source is "COLORFGBG" or "fallback".
	Source string
	// Detail explains the reading in a form fit for a diagnostic.
	Detail string
	// Confident is false when the mode is a guess rather than a reading.
	Confident bool
}

// DetectBackground infers the terminal background from the environment. lookup
// may be nil, in which case os.Getenv is used.
//
// Only COLORFGBG is consulted: querying the terminal over OSC 11 needs the
// terminal itself, which belongs to the TUI rather than here. Absent that, dark
// is assumed — the same fallback Pi uses.
func DetectBackground(lookup func(string) string) Detection {
	if lookup == nil {
		lookup = os.Getenv
	}
	if bg, ok := colorFgBgBackground(lookup("COLORFGBG")); ok {
		mode := Dark
		if rgb, err := ParseHex(ANSI256ToHex(bg)); err == nil && rgb.Luminance() >= 0.5 {
			mode = Light
		}
		return Detection{
			Mode:      mode,
			Source:    "COLORFGBG",
			Detail:    "background color index " + strconv.Itoa(bg),
			Confident: true,
		}
	}
	return Detection{
		Mode:   Dark,
		Source: "fallback",
		Detail: "no terminal background hint found",
	}
}

// colorFgBgBackground reads the background index out of COLORFGBG. The variable
// is "<fg>;<bg>" or "<fg>;<something>;<bg>", and either field may be a word
// like "default", so the last field that parses as 0-255 wins.
func colorFgBgBackground(colorfgbg string) (int, bool) {
	parts := strings.Split(colorfgbg, ";")
	for i := len(parts) - 1; i >= 0; i-- {
		n, ok := leadingInt(strings.TrimSpace(parts[i]))
		if ok && n >= 0 && n <= 255 {
			return n, true
		}
	}
	return 0, false
}

// leadingInt parses the integer a string starts with, matching what JavaScript's
// parseInt does with a trailing tail.
func leadingInt(s string) (int, bool) {
	end := 0
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		end = 1
	}
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
