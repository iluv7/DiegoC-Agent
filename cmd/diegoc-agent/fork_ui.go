package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"diegoc-agent/internal/agent"
	"github.com/mattn/go-runewidth"
	"github.com/peterh/liner"
	"golang.org/x/term"
)

func printForkEvents(manager *agent.ForkManager) bool {
	events, permissions := manager.Events()
	if len(events)+len(permissions) == 0 {
		return false
	}
	var output strings.Builder
	for _, event := range events {
		fmt.Fprintf(&output, "\n[%s] %s\n", event.SessionID, event.Status)
		if event.Result != "" {
			fmt.Fprintln(&output, terminalReport(event.Result))
		}
		if event.Error != "" {
			fmt.Fprintln(&output, terminalReport(event.Error))
		}
	}
	for _, p := range permissions {
		fmt.Fprintf(&output, "\n[%s] Approval required:\n", p.SessionID)
		for _, tc := range p.Request.ToolCalls {
			fmt.Fprintf(&output, "  %s %s\n", tc.Name, terminalReport(fmt.Sprint(tc.Args)))
		}
		fmt.Fprintf(&output, "/approve %s y|a|n (allow once / allow suggested rule / deny)\n", p.SessionID)
	}
	fmt.Print("\r\x1b[2K", strings.ReplaceAll(output.String(), "\n", "\r\n"))
	return true
}
func terminalReport(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}

// A single input owner handles the idle prompt and background events, preserving
// partially typed text. No worker or notification goroutine reads/writes the TTY.
func promptWithForkEvents(state *liner.State, manager *agent.ForkManager, history []string) (string, error) {
	return promptSessionInput(state, manager, history, "You › ", false)
}

func promptSessionInput(state *liner.State, manager *agent.ForkManager, history []string, prompt string, cancelOnEsc bool) (string, error) {
	return promptSessionInputUntil(state, manager, history, prompt, cancelOnEsc, nil, nil)
}

var errLoopDue = errors.New("scheduled loop is due")

type inputDraft struct {
	Text   string
	Cursor int
}

// Wakeups share the terminal's single input owner. Returning restores cooked
// mode before a scheduled Agent turn begins, preserving the draft and cursor.
func promptSessionInputUntil(state *liner.State, manager *agent.ForkManager, history []string, prompt string, cancelOnEsc bool, saved *inputDraft, ready func() bool) (string, error) {
	f, ok := stdinFile()
	if !ok || !term.IsTerminal(int(os.Stdout.Fd())) {
		printForkEvents(manager)
		return state.Prompt(prompt)
	}
	old, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return "", err
	}
	defer term.Restore(int(f.Fd()), old)
	fmt.Print("\x1b[?2004h")
	defer fmt.Print("\x1b[?2004l")
	var text []rune
	pos := 0
	if saved != nil {
		text = []rune(saved.Text)
		pos = saved.Cursor
		if pos < 0 || pos > len(text) {
			pos = len(text)
		}
		*saved = inputDraft{}
	}
	historyPos := len(history)
	draft := ""
	paste := false
	draw := func() {
		width, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || width < 20 {
			width = 80
		}
		// Keep the cursor visible without allowing the editable line to wrap.
		available := width - runewidth.StringWidth(prompt) - 2
		left := string(text[:pos])
		right := string(text[pos:])
		if runewidth.StringWidth(left) > available {
			runes := []rune(left)
			for len(runes) > 0 && runewidth.StringWidth(string(runes)) > available-1 {
				runes = runes[1:]
			}
			left = "…" + string(runes)
		}
		right = runewidth.Truncate(right, available-runewidth.StringWidth(left), "…")
		fmt.Printf("\r\x1b[2K%s%s%s", prompt, left, right)
		if n := runewidth.StringWidth(right); n > 0 {
			fmt.Printf("\x1b[%dD", n)
		}
	}
	draw()
	var encoded []byte
	lastInput := time.Now()
	for {
		if printForkEvents(manager) {
			draw()
		}
		// Give active typing priority and never split a paste or UTF-8 rune.
		if ready != nil && !paste && len(encoded) == 0 && time.Since(lastInput) >= time.Second && ready() {
			if saved != nil {
				*saved = inputDraft{Text: string(text), Cursor: pos}
			}
			fmt.Print("\r\x1b[2K\r\n")
			return "", errLoopDue
		}
		b, ok, err := readTerminalByte(f, 100)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		lastInput = time.Now()
		if b == 0x1b {
			seq := "\x1b"
			for len(seq) < 64 {
				next, present, err := readTerminalByte(f, 80)
				if err != nil {
					return "", err
				}
				if !present {
					break
				}
				seq += string(next)
				if len(seq) == 2 && next != '[' && next != 'O' {
					break
				}
				if len(seq) > 2 && ((next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') || next == '~') {
					break
				}
			}
			if seq == "\x1b" && cancelOnEsc {
				fmt.Print("\r\n")
				return "", liner.ErrPromptAborted
			}
			if seq == "\x1b[200~" {
				paste = true
				continue
			}
			if seq == "\x1b[201~" {
				paste = false
				continue
			}
			if paste {
				continue
			}
			switch seq {
			case "\x1b[D":
				if pos > 0 {
					pos--
				}
			case "\x1b[C":
				if pos < len(text) {
					pos++
				}
			case "\x1b[H", "\x1bOH":
				pos = 0
			case "\x1b[F", "\x1bOF":
				pos = len(text)
			case "\x1b[3~":
				if pos < len(text) {
					text = append(text[:pos], text[pos+1:]...)
				}
			case "\x1b[A":
				if historyPos == len(history) {
					draft = string(text)
				}
				if historyPos > 0 {
					historyPos--
					text = []rune(history[historyPos])
					pos = len(text)
				}
			case "\x1b[B":
				if historyPos < len(history) {
					historyPos++
					if historyPos == len(history) {
						text = []rune(draft)
					} else {
						text = []rune(history[historyPos])
					}
					pos = len(text)
				}
			}
			draw()
			continue
		}
		if paste && (b == '\r' || b == '\n') {
			b = ' '
		}
		switch b {
		case '\r', '\n':
			fmt.Print("\r\n")
			return string(text), nil
		case 3:
			fmt.Print("\r\n")
			return "", liner.ErrPromptAborted
		case 4:
			if len(text) == 0 {
				fmt.Print("\r\n")
				return "", io.EOF
			}
			if pos < len(text) {
				text = append(text[:pos], text[pos+1:]...)
			}
		case 1:
			pos = 0
		case 5:
			pos = len(text)
		case 11:
			text = text[:pos]
		case 21:
			text = append([]rune(nil), text[pos:]...)
			pos = 0
		case 127, 8:
			if pos > 0 {
				text = append(text[:pos-1], text[pos:]...)
				pos--
			}
		case '\t':
			matches := []string{}
			for _, cmd := range slashCommands {
				if strings.HasPrefix(cmd, string(text)) {
					matches = append(matches, cmd)
				}
			}
			if len(matches) == 1 {
				text = []rune(matches[0])
				pos = len(text)
			}
		default:
			if b < 32 {
				continue
			}
			encoded = append(encoded, b)
			if !utf8.FullRune(encoded) {
				continue
			}
			r, _ := utf8.DecodeRune(encoded)
			encoded = nil
			text = append(text, 0)
			copy(text[pos+1:], text[pos:])
			text[pos] = r
			pos++
		}
		draw()
	}
}

func handleBackgroundCommand(manager *agent.ForkManager, line string) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}
	switch strings.ToLower(parts[0]) {
	case "/tasks":
		if len(parts) > 2 {
			fmt.Println("Usage: /tasks [fork-ID]")
			return true
		}
		sessions := manager.List()
		if len(sessions) == 0 {
			fmt.Println("No fork sessions.")
		}
		for _, s := range sessions {
			if len(parts) == 2 && parts[1] != s.ID {
				continue
			}
			fmt.Printf("[%s] %s — %s\n", s.ID, s.Status, terminalReport(s.Task))
			if len(parts) == 2 {
				fmt.Println(terminalReport(s.Result))
				if s.Error != "" {
					fmt.Println(terminalReport(s.Error))
				}
			}
		}
		if len(parts) == 2 {
			messages, err := manager.History(parts[1])
			if err != nil {
				fmt.Println(err)
			} else {
				for _, msg := range messages {
					if msg.Role != "system" {
						fmt.Printf("%s: %s\n", msg.Role, terminalReport(fmt.Sprint(msg.Content)))
					}
				}
			}
		}
		for _, p := range manager.Pending() {
			fmt.Printf("[%s] awaiting /approve %s y|a|n\n", p.SessionID, p.SessionID)
		}
	case "/send":
		fields := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, parts[0])), " ", 2)
		if len(fields) != 2 {
			fmt.Println("Usage: /send fork-ID message")
			return true
		}
		if err := manager.Send(fields[0], strings.TrimSpace(fields[1])); err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("Message sent to", fields[0])
		}
	case "/cancel":
		if len(parts) != 2 {
			fmt.Println("Usage: /cancel fork-ID")
			return true
		}
		if err := manager.Cancel(parts[1]); err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("Cancellation requested for", parts[1])
		}
	case "/approve":
		if len(parts) != 3 || (parts[2] != "y" && parts[2] != "a" && parts[2] != "n") {
			fmt.Println("Usage: /approve fork-ID y|a|n")
			return true
		}
		if err := manager.Approve(parts[1], parts[2] != "n", parts[2] == "a"); err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("Permission response sent to", parts[1])
		}
	default:
		return false
	}
	return true
}
