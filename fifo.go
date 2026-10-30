package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// fifo turns each line written to a named pipe into a message, so any script
// can notify: echo "build done" 1<> ~/.config/opcom/notify.
type fifo struct {
	f *os.File
}

func newFIFO(path string) (*fifo, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("fifo: %w", err)
		}
		path = filepath.Join(home, rest)
	}

	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			return nil, fmt.Errorf("fifo: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("fifo: %w", err)
	case fi.Mode()&fs.ModeNamedPipe == 0:
		return nil, fmt.Errorf("fifo: %s exists and is not a named pipe", path)
	}

	// Holding the pipe open read-write keeps a writer attached, so reads block
	// between writers instead of hitting EOF. Linux and macOS allow this.
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the path is from the user's own config
	if err != nil {
		return nil, fmt.Errorf("fifo: %w", err)
	}
	return &fifo{f: f}, nil
}

func (p *fifo) run(send func(tea.Msg)) {
	sc := bufio.NewScanner(p.f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			send(Message{From: "LOCAL", Body: []string{line}})
		}
	}
	send(linkStatus{source: "LOCAL", detail: fmt.Sprint("fifo closed: ", sc.Err())})
}
