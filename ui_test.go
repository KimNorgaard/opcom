package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSelectAndOpen(t *testing.T) {
	var opened []string
	openURL = func(u string) error {
		opened = append(opened, u)
		return nil
	}

	m := newModel(schemes["green"], "READY.\n")
	m.w, m.h = 80, 24
	feed := func(msgs ...tea.Msg) {
		for _, msg := range msgs {
			next, cmd := m.Update(msg)
			m = next.(model)
			if cmd != nil {
				cmd()
			}
		}
	}
	key := func(s string) tea.Msg {
		switch s {
		case "up":
			return tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			return tea.KeyMsg{Type: tea.KeyDown}
		case "enter":
			return tea.KeyMsg{Type: tea.KeyEnter}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}

	feed(
		Message{From: "A", Body: []string{"one"}, URL: "https://a/1"},
		Message{From: "B", Body: []string{"no link"}},
		Message{From: "C", Body: []string{"bad"}, URL: "javascript:alert(1)"},
		Message{From: "D", Body: []string{"esc"}, URL: "https://d/\x1b]8;;"},
		Message{From: "E", Body: []string{"two"}, URL: "https://e/2"},
	)
	// Everything typed.
	m.n = len(m.text)
	m.born = make([]time.Time, m.n)
	if len(m.links) != 2 {
		t.Fatalf("links: %+v", m.links)
	}

	feed(key("enter"))
	feed(key("up"), key("up"), key("enter"))
	if m.sel != 0 {
		t.Fatalf("sel = %d, want 0", m.sel)
	}
	if !strings.Contains(m.View(), "\x1b]8;id=opcom0;https://a/1\x1b\\") {
		t.Fatal("no OSC 8 link in view")
	}
	feed(key("down"), key("down"))
	if m.sel != -1 {
		t.Fatalf("sel = %d, want live", m.sel)
	}
	if want := []string{"https://e/2", "https://a/1"}; !slices.Equal(opened, want) {
		t.Fatalf("opened %q, want %q", opened, want)
	}
}

func TestStatusNamesDownSources(t *testing.T) {
	m := newModel(schemes["green"], "")
	m.w = 80
	for _, src := range []string{"JIRA", "CALENDAR"} {
		next, _ := m.Update(linkStatus{source: src})
		m = next.(model)
	}
	if s := m.status(time.Now()); !strings.Contains(s, "NO CARRIER CALENDAR,JIRA") {
		t.Fatalf("status %q", s)
	}
}
