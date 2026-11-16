package evaluate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// checkReplacements refuses a Pome whose [replace] sends a dependency to a directory that is not a
// Pome. The path is relative to the directory of Pome.toml, so a Pome copied away from the checkout
// it was written for (an example copied out of this repository, a declaration whose repository does
// not hold the local Pome) points at nothing; emela then stops with only the operating system's
// words ("Pome.toml: No such file or directory"). This says what is wrong and what to do instead.
// An unreadable Pome.toml, and entries that are not a local path, are left to emela.
func checkReplacements(dir string) error {
	manifest := filepath.Join(dir, "Pome.toml")
	var pome struct {
		Replace map[string]struct {
			Path string `toml:"path"`
		} `toml:"replace"`
	}
	if _, err := toml.DecodeFile(manifest, &pome); err != nil {
		return nil
	}
	names := make([]string, 0, len(pome.Replace))
	for name := range pome.Replace {
		names = append(names, name)
	}
	sort.Strings(names) // the report must not depend on map order

	var problems []string
	for _, name := range names {
		path := pome.Replace[name].Path
		if path == "" {
			continue
		}
		target := path
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
			problems = append(problems, fmt.Sprintf("%s = { path = %q }: %s is not a directory", name, path, target))
		} else if fi, err := os.Stat(filepath.Join(target, "Pome.toml")); err != nil || fi.IsDir() {
			problems = append(problems, fmt.Sprintf("%s = { path = %q }: %s has no Pome.toml", name, path, target))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return &Error{Code: CodeReplaceMissing, Stage: StageBuild,
		Message: fmt.Sprintf("[replace] in %s points at %d local Pome(s) that are not there: a path is relative to the directory "+
			"of that Pome.toml, so it works only where the Pome sits next to the checkout it was written for", manifest, len(problems)),
		Hint: "copy the whole repository checkout instead of one directory of it, or correct the path in [replace], or remove the entry " +
			"to use the version named in [dependencies] (it must be in the Pome cache: emela pome install)",
		Detail: strings.Join(problems, "\n")}
}
