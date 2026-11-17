package site_test

// The tests of the core run with the node extensions Mamela is built with, as cmd/mamela and
// cmd/mamelet do: the core's own code does not import them (TestCoreDoesNotDependOnExtensions).
import _ "github.com/ieyoukan/mamela/internal/ext/k3s"
