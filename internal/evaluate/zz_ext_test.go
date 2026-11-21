package evaluate

// The tests of evaluation run with the node extensions Mamela is built with (the kinds that take a
// text from a file of the Pome are theirs), as cmd/mamela does: the package itself does not import
// them (TestCoreDoesNotDependOnExtensions).
import (
	_ "github.com/ieyoukan/mamela/internal/ext/k3s"
	_ "github.com/ieyoukan/mamela/internal/ext/linux"
)
