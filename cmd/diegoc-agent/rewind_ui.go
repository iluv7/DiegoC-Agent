package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// readTerminalByte polls rather than leaving a blocked reader behind after exit.
func readTerminalByte(f *os.File, timeout int) (byte, bool, error) {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeout)
	if err == unix.EINTR {
		return 0, false, nil
	}
	if err != nil || n == 0 {
		return 0, false, err
	}
	var buf [1]byte
	n, err = f.Read(buf[:])
	if n == 0 && err == nil {
		return 0, false, fmt.Errorf("terminal input closed")
	}
	return buf[0], n > 0, err
}

func selectorEvent(sequence string) string {
	switch sequence {
	case "\r", "\n":
		return "enter"
	case "\x03", "\x1b", "q":
		return "cancel"
	case "\x1b[A", "\x1bOA", "k":
		return "up"
	case "\x1b[B", "\x1bOB", "j":
		return "down"
	case "\x1b[5~":
		return "pageup"
	case "\x1b[6~":
		return "pagedown"
	case "\x1b[H":
		return "home"
	case "\x1b[F":
		return "end"
	}
	if strings.HasPrefix(sequence, "\x1b[<") && strings.HasSuffix(sequence, "M") {
		fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(sequence, "\x1b[<"), "M"), ";")
		if len(fields) == 3 {
			code, err := strconv.Atoi(fields[0])
			if err == nil {
				switch code {
				case 64:
					return "up"
				case 65:
					return "down"
				}
			}
		}
	}
	return ""
}

func readSelectorEvent(f *os.File) (string, error) {
	b, ok, err := readTerminalByte(f, -1)
	if err != nil || !ok {
		return "", err
	}
	sequence := string(b)
	if b == 0x1b {
		for len(sequence) < 64 {
			b, ok, err = readTerminalByte(f, 80)
			if err != nil {
				return "", err
			}
			if !ok {
				break
			}
			sequence += string(b)
			if len(sequence) == 2 && b != '[' && b != 'O' {
				break
			}
			if len(sequence) > 2 && ((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~') {
				break
			}
		}
	}
	return selectorEvent(sequence), nil
}

func selectorText(text string, width int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return runewidth.Truncate(text, width, "…")
}

// selectRewindOption owns terminal input only until selection/cancellation.
func selectRewindOption(title string, options []string, initial int) (int, error) {
	if len(options) == 0 {
		return -1, nil
	}
	f, ok := stdinFile()
	if !ok || !term.IsTerminal(int(os.Stdout.Fd())) {
		return -1, fmt.Errorf("interactive selector requires a terminal; use /rewind ID [both|conversation|files]")
	}
	old, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return -1, err
	}
	defer term.Restore(int(f.Fd()), old)
	fmt.Print("\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h")
	defer fmt.Print("\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l")
	selected := initial
	for {
		width, height, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			width, height = 80, 24
		}
		if width < 10 {
			width = 10
		}
		if height < 5 {
			height = 5
		}
		visible := height - 4
		start := selected - visible/2
		if start < 0 {
			start = 0
		}
		if start+visible > len(options) {
			start = len(options) - visible
		}
		if start < 0 {
			start = 0
		}
		fmt.Print("\x1b[H\x1b[2J")
		fmt.Printf("%s\r\nWheel / ↑ ↓: select · Enter: confirm · Esc: cancel\r\n\r\n", selectorText(title, width-1))
		for i := start; i < len(options) && i < start+visible; i++ {
			if i == selected {
				fmt.Printf("\x1b[7m› %s\x1b[0m\r\n", selectorText(options[i], width-3))
			} else {
				fmt.Printf("  %s\r\n", selectorText(options[i], width-3))
			}
		}
		event, err := readSelectorEvent(f)
		if err != nil {
			return -1, err
		}
		switch event {
		case "enter":
			return selected, nil
		case "cancel":
			return -1, nil
		case "up":
			selected--
		case "down":
			selected++
		case "pageup":
			selected -= visible
		case "pagedown":
			selected += visible
		case "home":
			selected = 0
		case "end":
			selected = len(options) - 1
		}
		if selected < 0 {
			selected = 0
		}
		if selected >= len(options) {
			selected = len(options) - 1
		}
	}
}
