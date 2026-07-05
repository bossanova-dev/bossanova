package accountflow

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Prompter is the terminal I/O seam used by the registration flows. Real
// implementations read from stdin and write to stdout; tests inject scripted
// answers.
type Prompter interface {
	// Say prints a user-visible line (newline appended).
	Say(format string, args ...any)
	// Ask reads a line of input, returning def when the user enters nothing.
	Ask(prompt, def string) (string, error)
	// AskSecret reads a secret. Input is masked via x/term when the reader is
	// the real terminal stdin; otherwise it is read as a plain line.
	AskSecret(prompt string) (string, error)
	// Confirm asks a yes/no question, returning def when the user enters nothing.
	Confirm(prompt string, def bool) (bool, error)
}

// NewIOPrompter returns a Prompter reading from in and writing to out.
func NewIOPrompter(in io.Reader, out io.Writer) Prompter {
	return &ioPrompter{in: in, r: bufio.NewReader(in), out: out}
}

type ioPrompter struct {
	in  io.Reader
	r   *bufio.Reader
	out io.Writer
}

func (p *ioPrompter) Say(format string, args ...any) {
	_, _ = fmt.Fprintf(p.out, format+"\n", args...)
}

func (p *ioPrompter) readLine() (string, error) {
	line, err := p.r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

func (p *ioPrompter) Ask(prompt, def string) (string, error) {
	if def != "" {
		_, _ = fmt.Fprintf(p.out, "%s [%s]: ", prompt, def)
	} else {
		_, _ = fmt.Fprintf(p.out, "%s: ", prompt)
	}
	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (p *ioPrompter) AskSecret(prompt string) (string, error) {
	_, _ = fmt.Fprintf(p.out, "%s: ", prompt)
	if f, ok := p.in.(*os.File); ok && f == os.Stdin && term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(p.out)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return p.readLine()
}

func (p *ioPrompter) Confirm(prompt string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	_, _ = fmt.Fprintf(p.out, "%s [%s]: ", prompt, hint)
	line, err := p.readLine()
	if err != nil {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def, nil
	}
	return line == "y" || line == "yes", nil
}

var _ Prompter = (*ioPrompter)(nil)
