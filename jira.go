package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	evAssigned = "assigned"
	// evMentioned is shared with the GitHub source.
)

type jiraConfig struct {
	URL      string   `json:"url"`
	Interval string   `json:"interval"`
	Events   []string `json:"events"` // empty means all
}

type jira struct {
	api      string
	token    string
	interval time.Duration
	events   map[string]bool
	me       string
	started  time.Time
	since    time.Time

	assigned map[string]bool // nil until the first poll
	seen     map[string]bool // mentions already reported

	// ponytail: seen grows for the lifetime of the process, like the GitHub
	// source's. Prune by age if this ever runs for months.
}

func newJira(cfg jiraConfig) (*jira, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("jira: url must be https://host, got %q", cfg.URL)
	}
	now := time.Now()
	j := &jira{
		api:      strings.TrimSuffix(cfg.URL, "/"),
		token:    os.Getenv("JIRA_API_TOKEN"),
		interval: time.Minute,
		events:   map[string]bool{},
		started:  now,
		since:    now,
		seen:     map[string]bool{},
	}
	if j.token == "" {
		return nil, fmt.Errorf("jira: JIRA_API_TOKEN is not set")
	}
	if cfg.Interval != "" {
		d, err := time.ParseDuration(cfg.Interval)
		if err != nil {
			return nil, fmt.Errorf("jira: interval: %w", err)
		}
		j.interval = d
	}
	evs := cfg.Events
	if len(evs) == 0 {
		evs = []string{evAssigned, evMentioned}
	}
	for _, e := range evs {
		if e != evAssigned && e != evMentioned {
			return nil, fmt.Errorf("jira: unknown event %q", e)
		}
		j.events[e] = true
	}
	return j, nil
}

func (j *jira) run(send func(tea.Msg)) {
	up := true
	for {
		err := j.poll(send)
		if (err == nil) != up {
			up = err == nil
			st := linkStatus{source: "JIRA", ok: up}
			if err != nil {
				st.detail = err.Error()
			}
			send(st)
		}
		time.Sleep(j.interval)
	}
}

// jiraTime parses Jira Server timestamps, whose zone offset has no colon.
type jiraTime struct{ time.Time }

func (t *jiraTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return err
	}
	var err error
	t.Time, err = time.Parse("2006-01-02T15:04:05.000-0700", s)
	return err
}

type jiraUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type jiraFields struct {
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Created     jiraTime `json:"created"`
	Creator     jiraUser `json:"creator"`
}

type jiraComment struct {
	ID      string   `json:"id"`
	Body    string   `json:"body"`
	Created jiraTime `json:"created"`
	Author  jiraUser `json:"author"`
}

type jiraIssue struct {
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

func (j *jira) browse(key string) string { return j.api + "/browse/" + key }

func (i jiraIssue) from() string {
	project, _, _ := strings.Cut(i.Key, "-")
	return "JIRA " + project
}

func (j *jira) poll(send func(tea.Msg)) error {
	start := time.Now()
	if j.me == "" {
		var u jiraUser
		if err := j.get("/rest/api/2/myself", &u); err != nil {
			return err
		}
		j.me = u.Name
	}
	if j.events[evAssigned] {
		if err := j.pollAssigned(send); err != nil {
			return err
		}
	}
	if j.events[evMentioned] {
		if err := j.pollMentions(send); err != nil {
			return err
		}
	}
	j.since = start
	return nil
}

// pollAssigned reports issues that newly appear in my unresolved set. The
// first poll reports only a count.
func (j *jira) pollAssigned(send func(tea.Msg)) error {
	const jql = "assignee = currentUser() AND resolution = Unresolved"
	issues, err := j.search(jql, "summary")
	if err != nil {
		return err
	}
	cur := map[string]bool{}
	for _, it := range issues {
		cur[it.Key] = true
		if j.assigned != nil && !j.assigned[it.Key] {
			send(Message{From: it.from(), Body: []string{it.Key + " assigned to you", it.Fields.Summary}, URL: j.browse(it.Key)})
		}
	}
	if j.assigned == nil && len(cur) > 0 {
		send(Message{From: "JIRA", Body: []string{fmt.Sprintf("%d issues assigned", len(cur))}, URL: j.api + "/issues/?jql=" + url.QueryEscape(jql)})
	}
	j.assigned = cur
	return nil
}

// pollMentions finds recently updated issues whose text contains my name,
// then looks for the [~name] markup in new comments and new descriptions.
// A relative JQL date avoids Jira's per-user timezone for absolute ones.
func (j *jira) pollMentions(send func(tea.Msg)) error {
	mins := int(time.Since(j.since).Minutes()) + 5
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(j.me)
	issues, err := j.search(fmt.Sprintf(`text ~ "%s" AND updated >= -%dm`, quoted, mins), "summary,description,created,creator")
	if err != nil {
		return err
	}
	tag := "[~" + j.me + "]"
	for _, it := range issues {
		key := "desc " + it.Key
		if it.Fields.Created.After(j.started) && strings.Contains(it.Fields.Description, tag) &&
			it.Fields.Creator.Name != j.me && !j.seen[key] {
			j.seen[key] = true
			send(Message{From: it.from(), Body: []string{it.Fields.Creator.DisplayName + " mentioned you in " + it.Key, it.Fields.Summary}, URL: j.browse(it.Key)})
		}

		var res struct {
			Comments []jiraComment `json:"comments"`
		}
		if err := j.get("/rest/api/2/issue/"+it.Key+"/comment?orderBy=-created&maxResults=50", &res); err != nil {
			return err
		}
		for _, c := range res.Comments {
			key := "comment " + c.ID
			if !c.Created.After(j.started) || c.Author.Name == j.me || !strings.Contains(c.Body, tag) || j.seen[key] {
				continue
			}
			j.seen[key] = true
			send(Message{From: it.from(), Body: []string{c.Author.DisplayName + " mentioned you in " + it.Key, it.Fields.Summary}, URL: j.browse(it.Key) + "?focusedCommentId=" + c.ID})
		}
	}
	return nil
}

// search pages through all results of jql.
func (j *jira) search(jql, fields string) ([]jiraIssue, error) {
	var all []jiraIssue
	for {
		var res struct {
			Issues []jiraIssue `json:"issues"`
			Total  int         `json:"total"`
		}
		q := url.Values{"jql": {jql}, "fields": {fields}, "maxResults": {"100"}, "startAt": {fmt.Sprint(len(all))}}
		if err := j.get("/rest/api/2/search?"+q.Encode(), &res); err != nil {
			return nil, err
		}
		all = append(all, res.Issues...)
		if len(res.Issues) == 0 || len(all) >= res.Total {
			return all, nil
		}
	}
}

func (j *jira) get(endpoint string, v any) error {
	req, err := http.NewRequest("GET", j.api+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+j.token)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", strings.SplitN(endpoint, "?", 2)[0], resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
