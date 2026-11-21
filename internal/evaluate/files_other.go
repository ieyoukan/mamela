//go:build !unix

package evaluate

// openFlags: nothing more where there are no named pipes to wait on.
const openFlags = 0
