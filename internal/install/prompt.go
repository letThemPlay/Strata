package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrInputClosed is returned when standard input ends during a prompt.
var ErrInputClosed = errors.New("input ended before all questions were answered; nothing was written")

// Prompter asks questions line by line. Reads are canceled with the
// context (Ctrl-C), even while waiting for input.
//
// A Prompter also implements io.Reader, returning one input line per
// Read, so that code expecting a reader (the erase confirmation) shares
// its input without losing buffered lines.
type Prompter struct {
	ctx     context.Context //nolint:containedctx // Bounds every read; see NewPrompter.
	out     io.Writer
	lines   chan lineResult
	pending []byte
}

type lineResult struct {
	line string
	err  error
}

// NewPrompter reads lines from in and writes prompts to out. Reading
// happens in a goroutine that ends when in does.
func NewPrompter(ctx context.Context, in io.Reader, out io.Writer) *Prompter {
	lines := make(chan lineResult)
	go func() {
		r := bufio.NewReader(in)
		for {
			s, err := r.ReadString('\n')
			if s != "" {
				lines <- lineResult{line: s}
			}
			if err != nil {
				lines <- lineResult{err: err}
				return
			}
		}
	}()
	return &Prompter{ctx: ctx, out: out, lines: lines}
}

// next returns the next raw line including its newline, if any.
func (p *Prompter) next() (string, error) {
	select {
	case <-p.ctx.Done():
		return "", fmt.Errorf("wizard: %w", p.ctx.Err())
	case r, ok := <-p.lines:
		if !ok {
			return "", io.EOF
		}
		if r.err != nil {
			close(p.lines)
			p.lines = nil
			return "", r.err
		}
		return r.line, nil
	}
}

// Read implements io.Reader.
func (p *Prompter) Read(b []byte) (int, error) {
	if len(p.pending) == 0 {
		if p.lines == nil {
			return 0, io.EOF
		}
		line, err := p.next()
		if err != nil {
			return 0, err
		}
		p.pending = []byte(line)
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

// Line reads one line without its line ending.
func (p *Prompter) Line() (string, error) {
	if p.lines == nil {
		return "", ErrInputClosed
	}
	line, err := p.next()
	if errors.Is(err, io.EOF) {
		return "", ErrInputClosed
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Say prints a line.
func (p *Prompter) Say(format string, args ...any) {
	_, _ = fmt.Fprintf(p.out, format+"\n", args...)
}

// Ask prompts until check accepts the trimmed answer. An empty answer
// selects def; check must also accept it (or reject it to make the
// question mandatory).
func (p *Prompter) Ask(question, def string, check func(string) error) (string, error) {
	for {
		if def != "" {
			_, _ = fmt.Fprintf(p.out, "%s [%s]: ", question, def)
		} else {
			_, _ = fmt.Fprintf(p.out, "%s: ", question)
		}
		answer, err := p.Line()
		if err != nil {
			return "", err
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			answer = def
		}
		if err := check(answer); err != nil {
			p.Say("  %v", err)
			continue
		}
		return answer, nil
	}
}

// Confirm asks a yes/no question.
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	answer, err := p.Ask(question+" ("+d+")", "", func(s string) error {
		switch strings.ToLower(s) {
		case "", "y", "yes", "n", "no":
			return nil
		}
		return errors.New("answer y or n")
	})
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return def, nil
}

// Choose shows a numbered menu and returns the chosen index. Items with
// a non-empty entry in disabled are shown with that reason and cannot be
// chosen. def is the default index, or -1 for none.
func (p *Prompter) Choose(title string, items, disabled []string, def int) (int, error) {
	p.Say("%s", title)
	for i, item := range items {
		if i < len(disabled) && disabled[i] != "" {
			p.Say("  -) %s\n       unavailable: %s", item, disabled[i])
			continue
		}
		p.Say("  %d) %s", i+1, item)
	}
	defStr := ""
	if def >= 0 {
		defStr = strconv.Itoa(def + 1)
	}
	answer, err := p.Ask("Choice", defStr, func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > len(items) {
			return fmt.Errorf("enter a number from the list")
		}
		if n-1 < len(disabled) && disabled[n-1] != "" {
			return fmt.Errorf("%d is unavailable: %s", n, disabled[n-1])
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(answer)
	return n - 1, nil
}
