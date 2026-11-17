//go:build !windows

package main

import (
	"os"
	"syscall"
)

// restart replaces this process with the program at executable (the version
// an update has just installed), in the same terminal, with the same
// arguments and environment.
func restart(executable string) {
	err := syscall.Exec(executable, os.Args, os.Environ())
	fatal("couldn't restart into the new version, start the bot again: %v", err)
}
