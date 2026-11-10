// Package version names the Mamela build, so records can say which version made them.
package version

import "runtime/debug"

// Current is the VCS revision this binary was built from ("+dirty" when the tree had changes), or
// "unknown" when the build has no VCS information (for example in tests).
func Current() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
