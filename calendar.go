package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/emersion/go-ical"
)

// calendarFeed reads either an ICS url or a Google calendar through gws.
type calendarFeed struct {
	Name string `json:"name"`
	URL  string `json:"url"` // secret iCal address, treat as a credential
	GWS  string `json:"gws"` // Google calendar ID, such as "primary"

	// Refresh overrides the calendar's refresh for this feed.
	Refresh string `json:"refresh"`
	every   time.Duration
}

type calendarConfig struct {
	Refresh   string         `json:"refresh"`
	Warn      []string       `json:"warn"`
	Email     string         `json:"email"` // skip meetings this attendee declined
	Calendars []calendarFeed `json:"calendars"`
}

type meeting struct {
	cal, uid, summary, location string
	start                       time.Time
}

type calendar struct {
	refresh time.Duration
	warn    []time.Duration // ascending
	email   string
	feeds   []calendarFeed

	meetings map[string][]meeting // per feed, last good fetch
	warned   map[string]bool

	// Per feed fetch schedule and current failure.
	due   map[string]time.Time
	fails map[string]int
	errs  map[string]error

	// ponytail: warned grows by a few keys per meeting. Prune past meetings
	// if this ever runs for months.
}

const (
	// horizon is how far ahead recurring meetings are expanded.
	horizon = 24 * time.Hour
	// maxBackoff caps retries of a failing feed. Google answers 429 to
	// secret iCal feeds fetched too often, without a Retry-After.
	maxBackoff = 4 * time.Hour
)

func newCalendar(cfg calendarConfig) (*calendar, error) {
	c := &calendar{
		refresh:  10 * time.Minute,
		email:    strings.ToLower(cfg.Email),
		feeds:    cfg.Calendars,
		meetings: map[string][]meeting{},
		warned:   map[string]bool{},
		due:      map[string]time.Time{},
		fails:    map[string]int{},
		errs:     map[string]error{},
	}
	if cfg.Refresh != "" {
		d, err := time.ParseDuration(cfg.Refresh)
		if err != nil {
			return nil, fmt.Errorf("calendar: refresh: %w", err)
		}
		c.refresh = d
	}
	warn := cfg.Warn
	if len(warn) == 0 {
		warn = []string{"10m", "1m"}
	}
	for _, w := range warn {
		d, err := time.ParseDuration(w)
		if err != nil || d <= 0 || d > horizon {
			return nil, fmt.Errorf("calendar: warn %q must be a duration between 0 and 24h", w)
		}
		c.warn = append(c.warn, d)
	}
	slices.Sort(c.warn)
	if len(c.feeds) == 0 {
		return nil, fmt.Errorf("calendar: no calendars configured")
	}
	for i, f := range c.feeds {
		c.feeds[i].every = c.refresh
		if f.Refresh != "" {
			d, err := time.ParseDuration(f.Refresh)
			if err != nil {
				return nil, fmt.Errorf("calendar: %s: refresh: %w", f.Name, err)
			}
			c.feeds[i].every = d
		}
		if f.Name == "" || (f.URL == "") == (f.GWS == "") {
			return nil, fmt.Errorf("calendar: each calendar needs a name and exactly one of url or gws")
		}
		if f.URL == "" {
			continue
		}
		if !isHTTP(f.URL) {
			return nil, fmt.Errorf("calendar: %s: url must be http(s)", f.Name)
		}
	}
	return c, nil
}

func (c *calendar) run(send func(tea.Msg)) {
	up := true
	for {
		now := time.Now()
		if err := c.fetch(now); (err == nil) != up {
			up = err == nil
			st := linkStatus{source: "CALENDAR", ok: up}
			if err != nil {
				st.detail = err.Error()
			}
			send(st)
		}
		c.check(now, send)
		time.Sleep(15 * time.Second)
	}
}

// fetch refreshes the feeds that are due and returns every feed's current
// error. A failing feed keeps its last good meetings, so warnings still fire,
// and its retry interval doubles up to maxBackoff so a rate limit can lift.
func (c *calendar) fetch(now time.Time) error {
	for _, f := range c.feeds {
		if now.Before(c.due[f.Name]) {
			continue
		}
		ms, err := c.fetchFeed(f, now)
		if err != nil {
			c.fails[f.Name]++
			c.errs[f.Name] = fmt.Errorf("%s: %w", f.Name, err)
			c.due[f.Name] = now.Add(min(f.every<<min(c.fails[f.Name], 10), maxBackoff))
			continue
		}
		delete(c.fails, f.Name)
		delete(c.errs, f.Name)
		c.due[f.Name] = now.Add(f.every)
		c.meetings[f.Name] = ms
	}
	var errs []error
	for _, f := range c.feeds {
		if err := c.errs[f.Name]; err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *calendar) fetchFeed(f calendarFeed, now time.Time) ([]meeting, error) {
	if f.GWS != "" {
		return fetchGWS(f.Name, f.GWS, now, now.Add(horizon))
	}
	cal, err := fetchICS(f.URL)
	if err != nil {
		return nil, err
	}
	return c.expand(f.Name, cal, now, now.Add(horizon)), nil
}

// fetchICS never puts the URL in an error, because it carries the secret.
func fetchICS(u string) (*ical.Calendar, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, errors.New("invalid url")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", req.URL.Host, resp.Status)
	}
	// Share and embed links answer 200 with the calendar web app.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil, fmt.Errorf("%s returned a web page, not iCal. Use the secret iCal address", req.URL.Host)
	}
	return ical.NewDecoder(resp.Body).Decode()
}

type instance struct {
	uid   string
	start int64
}

// expand turns events into meetings starting in [from, to). An event with a
// RECURRENCE-ID replaces one instance of its recurring master, so the master
// skips that instance and the replacement stands on its own.
func (c *calendar) expand(name string, cal *ical.Calendar, from, to time.Time) []meeting {
	events := cal.Events()
	replaced := map[instance]bool{}
	for _, e := range events {
		if rid := e.Props.Get(ical.PropRecurrenceID); rid != nil {
			if t, err := rid.DateTime(time.Local); err == nil {
				replaced[instance{uidOf(e), t.Unix()}] = true
			}
		}
	}

	var out []meeting
	for _, e := range events {
		start := e.Props.Get(ical.PropDateTimeStart)
		if start == nil || len(start.Value) == len("20060102") || c.skip(e) {
			continue // all-day, canceled or declined
		}
		first, err := start.DateTime(time.Local)
		if err != nil {
			continue
		}
		starts := []time.Time{first}
		if e.Props.Get(ical.PropRecurrenceID) == nil {
			set, err := e.RecurrenceSet(time.Local)
			if err != nil {
				continue
			}
			if set != nil {
				starts = set.Between(from, to, true)
			}
		}
		uid := uidOf(e)
		summary, _ := e.Props.Text(ical.PropSummary)
		location, _ := e.Props.Text(ical.PropLocation)
		for _, s := range starts {
			if s.Before(from) || !s.Before(to) {
				continue
			}
			if e.Props.Get(ical.PropRecurrenceID) == nil && replaced[instance{uid, s.Unix()}] {
				continue
			}
			out = append(out, meeting{cal: name, uid: uid, summary: summary, location: location, start: s})
		}
	}
	return out
}

func uidOf(e ical.Event) string {
	uid, _ := e.Props.Text(ical.PropUID)
	return uid
}

func (c *calendar) skip(e ical.Event) bool {
	if st, _ := e.Status(); st == ical.EventCancelled {
		return true
	}
	if c.email == "" {
		return false
	}
	for _, a := range e.Props.Values(ical.PropAttendee) {
		if strings.EqualFold(strings.TrimPrefix(strings.ToLower(a.Value), "mailto:"), c.email) {
			return strings.EqualFold(a.Params.Get(ical.ParamParticipationStatus), "DECLINED")
		}
	}
	return false
}

// check warns once per stage. The stage is the smallest warn lead time that
// has been reached, so starting opcom 30s before a meeting gives one warning,
// not one per lead time.
func (c *calendar) check(now time.Time, send func(tea.Msg)) {
	for _, ms := range c.meetings {
		for _, m := range ms {
			if !now.Before(m.start) {
				continue
			}
			i := slices.IndexFunc(c.warn, func(w time.Duration) bool { return !now.Before(m.start.Add(-w)) })
			if i < 0 {
				continue
			}
			key := fmt.Sprintf("%s %d %s", m.uid, m.start.Unix(), c.warn[i])
			if c.warned[key] {
				continue
			}
			c.warned[key] = true
			mins := int((m.start.Sub(now) + time.Minute - 1) / time.Minute)
			unit := "minutes"
			if mins == 1 {
				unit = "minute"
			}
			body := []string{fmt.Sprintf("in %d %s: %s", mins, unit, m.summary)}
			if m.location != "" {
				body = append(body, m.location)
			}
			send(Message{From: "CALENDAR " + m.cal, Body: body})
		}
	}
}
