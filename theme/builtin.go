package theme

import (
	"embed"
	"fmt"
	"sync"
)

// Pi's own dark.json and light.json, embedded verbatim (MIT © 2025 Mario
// Zechner — see THIRD-PARTY-NOTICES.md). Keeping the files byte-identical means
// a theme derived from one of them by editing vars stays loadable by both
// tools.
//
//go:embed builtin/dark.json builtin/light.json
var builtinFS embed.FS

// BuiltinNames lists the themes compiled into the binary, in the order they
// take precedence during discovery. A custom theme cannot shadow one of these,
// which is also Pi's behaviour.
var BuiltinNames = []string{"dark", "light"}

var (
	builtinOnce  sync.Once
	builtinCache map[string]*Theme
	builtinErr   error
)

func builtins() (map[string]*Theme, error) {
	builtinOnce.Do(func() {
		builtinCache = make(map[string]*Theme, len(BuiltinNames))
		for _, name := range BuiltinNames {
			data, err := builtinFS.ReadFile("builtin/" + name + ".json")
			if err != nil {
				builtinErr = fmt.Errorf("built-in theme %q: %w", name, err)
				return
			}
			t, err := Parse(data, "")
			if err != nil {
				builtinErr = fmt.Errorf("built-in theme %q: %w", name, err)
				return
			}
			builtinCache[t.Name] = t
		}
	})
	return builtinCache, builtinErr
}

// Builtin returns a compiled-in theme by name.
func Builtin(name string) (*Theme, bool) {
	all, err := builtins()
	if err != nil {
		return nil, false
	}
	t, ok := all[name]
	return t, ok
}
