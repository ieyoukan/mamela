//go:build unix

package evaluate

import "syscall"

// openFlags open a file of the Pome without waiting on it (a named pipe put in its place) and without
// taking a terminal.
const openFlags = syscall.O_NONBLOCK | syscall.O_NOCTTY
