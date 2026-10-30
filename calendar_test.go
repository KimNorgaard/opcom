package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/emersion/go-ical"
)

// Weekly standup on Mondays at 09:00 Copenhagen time. One Monday is excluded,
// one is moved to 10:00, one is cancelled, and a separate meeting is declined.
const testICS = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:test
BEGIN:VEVENT
UID:standup
DTSTART;TZID=Europe/Copenhagen:20261005T090000
DTEND;TZID=Europe/Copenhagen:20261005T091500
RRULE:FREQ=WEEKLY;BYDAY=MO
EXDATE;TZID=Europe/Copenhagen:20261012T090000
SUMMARY:Standup
LOCATION:Room 1
END:VEVENT
BEGIN:VEVENT
UID:standup
RECURRENCE-ID;TZID=Europe/Copenhagen:20261019T090000
DTSTART;TZID=Europe/Copenhagen:20261019T100000
DTEND;TZID=Europe/Copenhagen:20261019T101500
SUMMARY:Standup (moved)
END:VEVENT
BEGIN:VEVENT
UID:standup
RECURRENCE-ID;TZID=Europe/Copenhagen:20261026T090000
DTSTART;TZID=Europe/Copenhagen:20261026T090000
STATUS:CANCELLED
SUMMARY:Standup
END:VEVENT
BEGIN:VEVENT
UID:declined
DTSTART:20261019T120000Z
SUMMARY:Declined
ATTENDEE;PARTSTAT=DECLINED:mailto:Me@Example.com
END:VEVENT
BEGIN:VEVENT
UID:allday
DTSTART;VALUE=DATE:20261019
SUMMARY:Holiday
END:VEVENT
END:VCALENDAR
`

func TestCalendarExpand(t *testing.T) {
	cal, err := ical.NewDecoder(strings.NewReader(strings.ReplaceAll(testICS, "\n", "\r\n"))).Decode()
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCalendar(calendarConfig{Email: "me@example.com", Calendars: []calendarFeed{{Name: "WORK", URL: "https://x"}}})
	if err != nil {
		t.Fatal(err)
	}
	cph, _ := time.LoadLocation("Europe/Copenhagen")
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, cph)

	var got []string
	for _, m := range c.expand("WORK", cal, from, from.AddDate(0, 0, 29)) {
		got = append(got, m.start.In(cph).Format("01-02 15:04 ")+m.summary)
	}
	slices.Sort(got)
	// 10-12 is excluded and 10-26 cancelled. 11-02 is after the DST change
	// and must stay at 09:00 local.
	want := []string{"10-05 09:00 Standup", "10-19 10:00 Standup (moved)", "11-02 09:00 Standup"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCalendarWarnOncePerStage(t *testing.T) {
	c, err := newCalendar(calendarConfig{Calendars: []calendarFeed{{Name: "WORK", URL: "https://x"}}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	c.meetings["WORK"] = []meeting{{cal: "WORK", uid: "a", summary: "Standup", start: start}}

	var got []string
	send := func(msg tea.Msg) { got = append(got, msg.(Message).Body[0]) }
	for _, at := range []time.Duration{-11 * time.Minute, -10 * time.Minute, -9 * time.Minute, -30 * time.Second, -10 * time.Second, 0} {
		c.check(start.Add(at), send)
	}
	if want := []string{"in 10 minutes: Standup", "in 1 minute: Standup"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Starting inside the last stage gives one warning, not one per stage.
	got = nil
	c.warned = map[string]bool{}
	c.check(start.Add(-30*time.Second), send)
	if len(got) != 1 {
		t.Fatalf("got %q", got)
	}
}

func TestParseGWS(t *testing.T) {
	data := `{"items":[
		{"id":"a","summary":"Standup","location":"Room 1","status":"confirmed","eventType":"default","start":{"dateTime":"2026-10-28T09:00:00+01:00"},"attendees":[{"self":true,"responseStatus":"accepted"}]},
		{"id":"b","summary":"Declined","status":"confirmed","start":{"dateTime":"2026-10-28T10:00:00+01:00"},"attendees":[{"self":true,"responseStatus":"declined"}]},
		{"id":"c","summary":"Holiday","status":"confirmed","start":{"date":"2026-10-28"}},
		{"id":"d","summary":"Office","status":"confirmed","eventType":"workingLocation","start":{"dateTime":"2026-10-28T08:00:00+01:00"}},
		{"id":"e","summary":"Gone","status":"cancelled","start":{"dateTime":"2026-10-28T11:00:00+01:00"}}]}`
	ms, err := parseGWS("WORK", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].summary != "Standup" || ms[0].start.UTC().Hour() != 8 {
		t.Fatalf("got %+v", ms)
	}
}

func TestCalendarBackoff(t *testing.T) {
	status := http.StatusTooManyRequests
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(status)
		fmt.Fprint(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nEND:VCALENDAR\r\n")
	}))
	defer srv.Close()

	c, err := newCalendar(calendarConfig{Refresh: "1m", Calendars: []calendarFeed{{Name: "HOME", URL: srv.URL, Refresh: "10m"}}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	// Retries at 20m and 60m after failing, then the normal 10m once it succeeds.
	for _, step := range []struct {
		at      time.Duration
		ok      bool
		wantHit int
	}{
		{0, false, 1},
		{19 * time.Minute, false, 1},
		{20 * time.Minute, false, 2},
		{59 * time.Minute, false, 2},
		{60 * time.Minute, true, 3},
		{70 * time.Minute, true, 4},
	} {
		if step.ok {
			status = http.StatusOK
		}
		err := c.fetch(start.Add(step.at))
		if hits != step.wantHit || (err == nil) != step.ok {
			t.Fatalf("at %s: hits %d err %v, want hits %d ok %t", step.at, hits, err, step.wantHit, step.ok)
		}
	}
}
