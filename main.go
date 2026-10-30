package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type config struct {
	Scheme   string          `json:"scheme"`
	GitHub   *githubConfig   `json:"github"`
	FIFO     string          `json:"fifo"`
	Jira     *jiraConfig     `json:"jira"`
	Calendar *calendarConfig `json:"calendar"`
}

func loadConfig(path string) (config, error) {
	cfg := config{Scheme: "green"}
	f, err := os.Open(path) // #nosec G304 -- the path is the user's own -config flag
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func main() {
	home, _ := os.UserHomeDir()
	path := flag.String("config", filepath.Join(home, ".config", "opcom", "config.json"), "config file")
	flag.Parse()

	if err := run(*path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	sc, ok := schemes[cfg.Scheme]
	if !ok {
		return fmt.Errorf("unknown scheme %q, want green or amber", cfg.Scheme)
	}

	// Each source is a constructor that validates config up front and a run
	// loop that pushes Message and linkStatus values into the program.
	var names []string
	var starts []func(func(tea.Msg))
	if cfg.GitHub != nil {
		gh, err := newGitHub(*cfg.GitHub)
		if err != nil {
			return err
		}
		names = append(names, "GITHUB")
		starts = append(starts, gh.run)
	}
	if cfg.FIFO != "" {
		f, err := newFIFO(cfg.FIFO)
		if err != nil {
			return err
		}
		names = append(names, "LOCAL")
		starts = append(starts, f.run)
	}
	if cfg.Jira != nil {
		j, err := newJira(*cfg.Jira)
		if err != nil {
			return err
		}
		names = append(names, "JIRA")
		starts = append(starts, j.run)
	}
	if cfg.Calendar != nil {
		c, err := newCalendar(*cfg.Calendar)
		if err != nil {
			return err
		}
		names = append(names, "CALENDAR")
		starts = append(starts, c.run)
	}
	if len(starts) == 0 {
		return fmt.Errorf("%s: no sources configured", path)
	}

	banner := "OPCOM V0.1\nSOURCES: " + strings.Join(names, ", ") + "\nREADY.\n"
	p := tea.NewProgram(newModel(sc, banner), tea.WithAltScreen())
	for _, start := range starts {
		go start(p.Send)
	}
	_, err = p.Run()
	return err
}
