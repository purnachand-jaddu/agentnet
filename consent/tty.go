package consent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// TTY prompts a person at a terminal.
type TTY struct {
	In  io.Reader
	Out io.Writer
}

// Confirm approves only when the person types exactly "yes".
func (t TTY) Confirm(ctx context.Context, title, detail string) (bool, error) {
	fmt.Fprintf(t.Out, "%s\n%s\nType yes to approve: ", title, detail)
	line, err := readLine(t.In)
	if err != nil {
		return false, err
	}
	return line == "yes", nil
}

// Choose prints a numbered list and returns the chosen option, or "" on invalid input.
func (t TTY) Choose(ctx context.Context, title, detail string, options []string) (string, error) {
	fmt.Fprintf(t.Out, "%s\n%s\n", title, detail)
	for i, o := range options {
		fmt.Fprintf(t.Out, "%d) %s\n", i+1, o)
	}
	fmt.Fprint(t.Out, "Choose a number: ")
	line, err := readLine(t.In)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return "", nil
	}
	return options[n-1], nil
}

// readLine reads one line from r, trimmed of its trailing newline.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
