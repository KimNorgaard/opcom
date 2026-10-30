package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// fetchGWS reads a Google calendar through the gws CLI, which owns the OAuth
// token. Google expands recurring meetings because of singleEvents.
func fetchGWS(name, calendarID string, from, to time.Time) ([]meeting, error) {
	params, err := json.Marshal(map[string]any{
		"calendarId":   calendarID,
		"singleEvents": true,
		"orderBy":      "startTime",
		"timeMin":      from.UTC().Format(time.RFC3339),
		"timeMax":      to.UTC().Format(time.RFC3339),
		"maxResults":   250,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gws", "calendar", "events", "list", "--params", string(params)).Output() // #nosec G204 -- fixed binary, args from the user's own config
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n")
			return nil, fmt.Errorf("gws: %s", lines[len(lines)-1])
		}
		return nil, fmt.Errorf("gws: %w", err)
	}
	return parseGWS(name, out)
}

type gwsTime struct {
	DateTime time.Time `json:"dateTime"` // zero for all-day events
}

type gwsAttendee struct {
	Self           bool   `json:"self"`
	ResponseStatus string `json:"responseStatus"`
}

type gwsEvent struct {
	ID        string        `json:"id"`
	Summary   string        `json:"summary"`
	Location  string        `json:"location"`
	Status    string        `json:"status"`
	EventType string        `json:"eventType"`
	Start     gwsTime       `json:"start"`
	Attendees []gwsAttendee `json:"attendees"`
}

const gwsCancelled = "cancelled" //nolint:misspell // Google's spelling of the status value

func parseGWS(name string, data []byte) ([]meeting, error) {
	var res struct {
		Items []gwsEvent `json:"items"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("gws: %w", err)
	}
	var out []meeting
	for _, e := range res.Items {
		if e.Start.DateTime.IsZero() || e.Status == gwsCancelled || e.EventType == "workingLocation" || e.EventType == "outOfOffice" {
			continue
		}
		declined := false
		for _, a := range e.Attendees {
			declined = declined || (a.Self && a.ResponseStatus == "declined")
		}
		if declined {
			continue
		}
		out = append(out, meeting{cal: name, uid: e.ID, summary: e.Summary, location: e.Location, start: e.Start.DateTime})
	}
	return out, nil
}
