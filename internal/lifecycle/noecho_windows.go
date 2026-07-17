//go:build windows

package lifecycle

import (
	"os"

	"golang.org/x/sys/windows"
)

// stdTerminal is the production terminalControl over os.Stdin using the Windows
// console mode (getpass parity, spec 01§6.1): ENABLE_ECHO_INPUT is cleared for
// the read and restored afterwards.
type stdTerminal struct{}

func (stdTerminal) isTerminal() bool {
	var mode uint32
	err := windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode)
	return err == nil
}

func (stdTerminal) disableEcho() (func() error, error) {
	h := windows.Handle(os.Stdin.Fd())
	var orig uint32
	if err := windows.GetConsoleMode(h, &orig); err != nil {
		return nil, err
	}
	raw := orig &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(h, raw); err != nil {
		return nil, err
	}
	return func() error {
		return windows.SetConsoleMode(h, orig)
	}, nil
}
