//go:build windows

package main

import (
	"os"
	"os/exec"
)

// restart starts the program at executable (the version an update has just
// installed) in this same console window, then exits. Windows can't swap a
// running process for another program the way Linux can.
func restart(executable string) {
	next := exec.Command(executable, os.Args[1:]...)
	next.Stdin, next.Stdout, next.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := next.Start(); err != nil {
		fatal("couldn't restart into the new version, start the bot again: %v", err)
	}
	os.Exit(0)
}
