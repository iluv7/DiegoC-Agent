package main

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/term"
)

func runWithEscCancel(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	f, ok := stdinFile()
	if !ok {
		return
	}
	fd := int(f.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	defer term.Restore(fd, oldState)
	for {
		if ctx.Err() != nil {
			return
		}
		b, ok, err := readTerminalByte(f, 100)
		if err != nil {
			return
		}
		if ok && b == 0x1b {
			fmt.Printf("\n%s⏹  Esc pressed, cancelling...%s\n", cBrightYellow, cReset)
			cancel()
			return
		}
	}
}

func stdinFile() (*os.File, bool) {
	f := os.Stdin
	if f == nil {
		return nil, false
	}
	stat, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if stat.Mode()&os.ModeCharDevice == 0 {
		return nil, false
	}
	return f, true
}
