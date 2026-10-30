package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify")
	p, err := newFIFO(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan tea.Msg, 1)
	go p.run(func(msg tea.Msg) { got <- msg })

	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("\n  build done  \n"); err != nil {
		t.Fatal(err)
	}
	w.Close()

	select {
	case msg := <-got:
		if m, ok := msg.(Message); !ok || m.Body[0] != "build done" {
			t.Fatalf("got %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no message")
	}
}

func TestPrintDropsControlChars(t *testing.T) {
	var m model
	m.print("a\x1b[2Jb\tc\n")
	if got := string(m.text); got != "A[2JBC\n" {
		t.Fatalf("got %q", got)
	}
}
