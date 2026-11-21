//go:build unix

package sumfile

import "syscall"

// openFlags open mamela.sum without waiting on it (a named pipe put in its place after it was looked
// at) and without taking a terminal.
const openFlags = syscall.O_NONBLOCK | syscall.O_NOCTTY
