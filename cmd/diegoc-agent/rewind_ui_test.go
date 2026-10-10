package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestSelectorEvents(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"\x1b[<64;12;5M", "up"}, {"\x1b[<65;12;5M", "down"},
		{"\x1b[<0;12;5M", ""}, {"\x1b[<65;12;5m", ""},
		{"\x1b[A", "up"}, {"\x1b[B", "down"}, {"\x1b[5~", "pageup"},
		{"\r", "enter"}, {"\x1b", "cancel"}, {"\x03", "cancel"},
	} {
		if got := selectorEvent(tc.input); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.input, got, tc.want)
		}
	}
}
func TestSelectorSanitizesPrompt(t *testing.T) {
	got := selectorText("hello\x1b[2J\nworld", 80)
	if strings.ContainsAny(got, "\x1b\n") {
		t.Fatal("terminal control characters survived")
	}
}

// Invoked by a PTY harness to exercise real raw-mode input and cleanup.
func TestSelectorTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_SELECTOR_TEST") != "1" {
		t.Skip("PTY helper")
	}
	options := make([]string, 30)
	for i := range options {
		options[i] = fmt.Sprintf("message %d", i)
	}
	selected, err := selectRewindOption("Select message", options, 29)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("SELECTED=%d\n", selected)
}
