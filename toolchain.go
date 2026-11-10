// Package mamela holds repository files that the commands embed. The code is in cmd/ and internal/.
package mamela

import _ "embed"

// ToolchainLock is toolchain.lock: the pinned emela and wasmtime (read by internal/toolchain).
//
//go:embed toolchain.lock
var ToolchainLock []byte
