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

func TestGitHubPoll(t *testing.T) {
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	routes := map[string]string{
		"/user":              `{"login":"me"}`,
		"/repos/o/r/pulls/2": `{"merged_by":{"login":"bob"}}`,
		"/repos/o/r/pulls/2/reviews": `[
			{"id":7,"state":"APPROVED","submitted_at":"` + later + `","user":{"login":"bob"}},
			{"id":8,"state":"COMMENTED","submitted_at":"` + later + `","user":{"login":"me"}},
			{"id":9,"state":"PENDING","user":{"login":"carol"}}]`,
		"/repos/o/r/pulls/3/reviews": `[]`,
		"/repos/o/r/pulls/3":         `{"merged_by":{"login":"me"}}`,
		"/notifications": `[
			{"id":"n1","reason":"mention","updated_at":"` + later + `","subject":{"title":"ping","url":"x/repos/o/r/issues/6","type":"Issue"},"repository":{"full_name":"o/r"}},
			{"id":"n2","reason":"subscribed","updated_at":"` + later + `","subject":{"title":"noise","url":"x/repos/o/r/issues/7","type":"Issue"},"repository":{"full_name":"o/r"}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			q := r.URL.Query().Get("q")
			switch {
			case strings.Contains(q, "status:failure"):
				fmt.Fprint(w, `{"items":[{"number":4,"title":"broken","repository_url":"x/repos/o/r"}]}`)
			case strings.Contains(q, "review-requested"):
				fmt.Fprint(w, `{"items":[
					{"number":1,"title":"fix","repository_url":"x/repos/o/r","user":{"login":"alex"}},
					{"number":5,"title":"other","repository_url":"x/repos/o/unwatched","user":{"login":"alex"}},
					{"number":8,"title":"bump deps","repository_url":"x/repos/o/r","user":{"login":"renovate[bot]","type":"Bot"}}]}`)
			default:
				fmt.Fprint(w, `{"items":[
					{"number":2,"title":"feat","html_url":"https://github.com/o/r/pull/2","repository_url":"x/repos/o/r","pull_request":{"merged_at":"`+later+`"}},
					{"number":3,"title":"mine","repository_url":"x/repos/o/r","pull_request":{"merged_at":"`+later+`"}}]}`)
			}
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	t.Setenv("GITHUB_TOKEN", "x")
	g, err := newGitHub(githubConfig{Repos: []githubRepo{{Repo: "O/R"}}})
	if err != nil {
		t.Fatal(err)
	}
	g.api = srv.URL

	var got, urls []string
	send := func(msg tea.Msg) {
		m := msg.(Message)
		got = append(got, m.Body[0])
		urls = append(urls, m.URL)
	}

	if err := g.poll(send); err != nil {
		t.Fatal(err)
	}
	want := []string{"1 review requests pending", "1 PRs failing CI", "bob approved #2", "bob merged #2", "you were mentioned in #6"}
	if !slices.Equal(got, want) {
		t.Fatalf("first poll: got %q, want %q", got, want)
	}
	wantURLs := []string{"https://github.com/pulls/review-requested", "", "https://github.com/o/r/pull/2", "https://github.com/o/r/pull/2", "https://github.com/o/r/issues/6"}
	if !slices.Equal(urls, wantURLs) {
		t.Fatalf("first poll urls: got %q, want %q", urls, wantURLs)
	}

	got = nil
	if err := g.poll(send); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("second poll repeated: %q", got)
	}

	for _, w := range g.watches {
		w.prev = map[string]bool{}
	}
	if err := g.poll(send); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alex requests a review of #1", "CI failed on #4"}; !slices.Equal(got, want) {
		t.Fatalf("new request: got %q, want %q", got, want)
	}
}

func TestGitHubWantsOrg(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	g, err := newGitHub(githubConfig{Repos: []githubRepo{
		{Org: "Acme"},
		{Repo: "acme/quiet", Events: []string{evMerged}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		repo, ev string
		want     bool
	}{
		{"acme/any", evReviewed, true},
		{"ACME/Any", evMerged, true},
		{"acme/quiet", evReviewed, false},
		{"acme/quiet", evMerged, true},
		{"other/any", evMerged, false},
	} {
		if got := g.wants(c.repo, c.ev); got != c.want {
			t.Errorf("wants(%q, %q) = %v, want %v", c.repo, c.ev, got, c.want)
		}
	}
}
