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
)

func TestJiraPoll(t *testing.T) {
	const layout = "2006-01-02T15:04:05.000-0700"
	later := time.Now().Add(time.Minute).Format(layout)
	earlier := time.Now().Add(-time.Hour).Format(layout)

	assigned := `{"key":"OPS-1","fields":{"summary":"rotate certs"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/2/myself":
			fmt.Fprint(w, `{"name":"me@x","displayName":"Me"}`)
		case "/rest/api/2/search":
			if strings.Contains(r.URL.Query().Get("jql"), "assignee") {
				fmt.Fprintf(w, `{"total":1,"issues":[%s]}`, assigned)
			} else {
				fmt.Fprint(w, `{"total":1,"issues":[{"key":"OPS-2","fields":{"summary":"outage","created":"`+earlier+`","description":"[~me@x] old"}}]}`)
			}
		case "/rest/api/2/issue/OPS-2/comment":
			fmt.Fprint(w, `{"comments":[
				{"id":"1","body":"ping [~me@x]","created":"`+later+`","author":{"name":"bob","displayName":"Bob"}},
				{"id":"2","body":"self [~me@x]","created":"`+later+`","author":{"name":"me@x"}},
				{"id":"3","body":"old [~me@x]","created":"`+earlier+`","author":{"name":"bob"}},
				{"id":"4","body":"no tag me@x","created":"`+later+`","author":{"name":"bob"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("JIRA_API_TOKEN", "x")
	j, err := newJira(jiraConfig{URL: "https://jira.example"})
	if err != nil {
		t.Fatal(err)
	}
	j.api = srv.URL

	var got []string
	var last Message
	send := func(msg tea.Msg) {
		last = msg.(Message)
		got = append(got, last.Body[0])
	}
	poll := func(want ...string) {
		t.Helper()
		got = nil
		if err := j.poll(send); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q, want %q", got, want)
		}
	}

	poll("1 issues assigned", "Bob mentioned you in OPS-2")
	poll()
	assigned += `,{"key":"OPS-3","fields":{"summary":"new"}}`
	poll("OPS-3 assigned to you")
	if want := srv.URL + "/browse/OPS-3"; last.URL != want {
		t.Fatalf("url: got %q, want %q", last.URL, want)
	}
}
