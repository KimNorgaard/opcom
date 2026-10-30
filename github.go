package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	evReviewRequested = "review_requested"
	evReviewed        = "reviewed"
	evMerged          = "merged"
	evCIFailed        = "ci_failed"
	evMentioned       = "mentioned"
)

var allEvents = []string{evReviewRequested, evReviewed, evMerged, evCIFailed, evMentioned}

// githubRepo watches either one repo or every repo in an org. A repo entry
// takes precedence over its org's entry.
type githubRepo struct {
	Repo   string   `json:"repo"`
	Org    string   `json:"org"`
	Events []string `json:"events"` // empty means all
}

type githubConfig struct {
	Interval string       `json:"interval"`
	Repos    []githubRepo `json:"repos"`
}

type github struct {
	api      string
	token    string
	interval time.Duration
	events   map[string]map[string]bool // lowercased owner/repo or org -> enabled events
	me       string
	started  time.Time
	since    time.Time

	watches []*watch
	seen    map[string]bool // reviews, merges and mentions already reported

	// ponytail: seen grows for the lifetime of the process, a few bytes per
	// event. Prune by age if this ever runs for months.
}

func newGitHub(cfg githubConfig) (*github, error) {
	now := time.Now()
	g := &github{
		api:      "https://api.github.com",
		interval: time.Minute,
		events:   map[string]map[string]bool{},
		started:  now,
		since:    now,
		seen:     map[string]bool{},
		watches: []*watch{
			{
				q: "is:pr is:open archived:false review-requested:@me", ev: evReviewRequested, pending: "review requests pending",
				pendingURL: "https://github.com/pulls/review-requested",
				// Matches requests to me and to my teams. The search result has
				// no requester, so the PR author stands in for it.
				line: func(it ghIssue) string { return fmt.Sprintf("%s requests a review of #%d", it.User.Login, it.Number) },
			},
			{
				q: "is:pr is:open archived:false author:@me status:failure", ev: evCIFailed, pending: "PRs failing CI",
				line: func(it ghIssue) string { return fmt.Sprintf("CI failed on #%d", it.Number) },
			},
		},
	}
	if cfg.Interval != "" {
		d, err := time.ParseDuration(cfg.Interval)
		if err != nil {
			return nil, fmt.Errorf("github: interval: %w", err)
		}
		g.interval = d
	}
	for _, r := range cfg.Repos {
		name := r.Repo
		switch {
		case (r.Repo == "") == (r.Org == ""):
			return nil, fmt.Errorf("github: each entry needs exactly one of repo or org")
		case r.Org != "":
			name = r.Org
			if strings.Contains(name, "/") {
				return nil, fmt.Errorf("github: org %q must not contain /", name)
			}
		case !strings.Contains(name, "/"):
			return nil, fmt.Errorf("github: repo %q must be owner/name", name)
		}
		evs := r.Events
		if len(evs) == 0 {
			evs = allEvents
		}
		set := map[string]bool{}
		for _, e := range evs {
			switch e {
			case evReviewRequested, evReviewed, evMerged, evCIFailed, evMentioned:
				set[e] = true
			default:
				return nil, fmt.Errorf("github: %s: unknown event %q", name, e)
			}
		}
		g.events[strings.ToLower(name)] = set
	}
	if len(g.events) == 0 {
		return nil, fmt.Errorf("github: no repos configured")
	}

	g.token = os.Getenv("GITHUB_TOKEN")
	if g.token == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err != nil {
			return nil, fmt.Errorf("github: set GITHUB_TOKEN or log in with gh: %w", err)
		}
		g.token = strings.TrimSpace(string(out))
	}
	return g, nil
}

func (g *github) wants(repo, ev string) bool {
	repo = strings.ToLower(repo)
	if set, ok := g.events[repo]; ok {
		return set[ev]
	}
	org, _, _ := strings.Cut(repo, "/")
	return g.events[org][ev]
}

func (g *github) run(send func(tea.Msg)) {
	up := true
	for {
		err := g.poll(send)
		if (err == nil) != up {
			up = err == nil
			st := linkStatus{source: "GITHUB", ok: up}
			if err != nil {
				st.detail = err.Error()
			}
			send(st)
		}
		time.Sleep(g.interval)
	}
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "Bot" for app accounts such as renovate
}

type ghPR struct {
	MergedAt *time.Time `json:"merged_at"`
}

type ghIssue struct {
	Number        int    `json:"number"`
	Title         string `json:"title"`
	HTMLURL       string `json:"html_url"`
	RepositoryURL string `json:"repository_url"`
	User          ghUser `json:"user"`
	PullRequest   ghPR   `json:"pull_request"`
}

func (i ghIssue) repo() string {
	p := strings.Split(i.RepositoryURL, "/")
	return p[len(p)-2] + "/" + p[len(p)-1]
}

// poll searches across all of GitHub and filters by repo locally, so the
// search rate limit (30/min) is spent per poll, not per configured repo.
func (g *github) poll(send func(tea.Msg)) error {
	start := time.Now()
	if g.me == "" {
		var u ghUser
		if err := g.get("/user", &u); err != nil {
			return err
		}
		g.me = u.Login
	}

	for _, w := range g.watches {
		if err := g.snapshot(w, send); err != nil {
			return err
		}
	}

	// The overlap absorbs search index lag. seen drops the repeats.
	mine, err := g.search("is:pr author:@me updated:>=" + g.since.Add(-5*time.Minute).UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	for _, it := range mine {
		repo := it.repo()
		if g.wants(repo, evReviewed) {
			if err := g.reviews(repo, it, send); err != nil {
				return err
			}
		}
		if g.wants(repo, evMerged) {
			if err := g.merged(repo, it, send); err != nil {
				return err
			}
		}
	}
	if err := g.mentions(send); err != nil {
		return err
	}
	g.since = start
	return nil
}

// watch is a search whose matching PRs are reported when they first appear.
type watch struct {
	q, ev      string
	pending    string // first-poll summary, after the count
	pendingURL string
	line       func(ghIssue) string
	prev       map[string]bool // nil until the first poll
}

// snapshot reports PRs that newly match w. The first poll reports only a
// count, because team review requests can leave hundreds pending.
func (g *github) snapshot(w *watch, send func(tea.Msg)) error {
	items, err := g.search(w.q)
	if err != nil {
		return err
	}
	cur := map[string]bool{}
	for _, it := range items {
		repo := it.repo()
		// Bots such as renovate request reviews from whole teams by the
		// hundred. Search has no qualifier for "any bot", so filter here.
		//
		// ponytail: bot PRs still cost search pages each poll. Add -author:
		// qualifiers for the worst offenders if the secondary rate limit bites.
		if !g.wants(repo, w.ev) || it.User.Type == "Bot" {
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, it.Number)
		cur[key] = true
		if w.prev != nil && !w.prev[key] {
			send(Message{From: "GITHUB " + repo, Body: []string{w.line(it), it.Title}, URL: it.HTMLURL})
		}
	}
	if w.prev == nil && len(cur) > 0 {
		send(Message{From: "GITHUB", Body: []string{fmt.Sprintf("%d %s", len(cur), w.pending)}, URL: w.pendingURL})
	}
	// Replacing the set means a PR that leaves and re-enters it, such as a
	// re-requested review or a new failing push, is reported again.
	w.prev = cur
	return nil
}

type ghSubject struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Type  string `json:"type"`
}

type ghRepoRef struct {
	FullName string `json:"full_name"`
}

// webKind maps a notification subject type to its path segment on github.com.
var webKind = map[string]string{"Issue": "issues", "PullRequest": "pull"}

// mentions reads the notifications API, because search can only tell that I
// was mentioned in a thread at some point, not when. Requires a classic or
// OAuth token: fine-grained tokens can't read notifications.
func (g *github) mentions(send func(tea.Msg)) error {
	var ns []struct {
		ID         string    `json:"id"`
		Reason     string    `json:"reason"`
		UpdatedAt  time.Time `json:"updated_at"`
		Subject    ghSubject `json:"subject"`
		Repository ghRepoRef `json:"repository"`
	}
	since := g.since.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	if err := g.get("/notifications?participating=true&all=true&per_page=100&since="+since, &ns); err != nil {
		return err
	}
	for _, n := range ns {
		repo := n.Repository.FullName
		// A thread's updated_at moves with each new mention, so each one is reported.
		key := fmt.Sprintf("mention %s %d", n.ID, n.UpdatedAt.Unix())
		onIssue := n.Subject.Type == "Issue" || n.Subject.Type == "PullRequest"
		if n.Reason != "mention" || !onIssue || !g.wants(repo, evMentioned) || !n.UpdatedAt.After(g.started) || g.seen[key] {
			continue
		}
		g.seen[key] = true
		send(Message{
			From: "GITHUB " + repo,
			Body: []string{"you were mentioned in #" + path.Base(n.Subject.URL), n.Subject.Title},
			URL:  "https://github.com/" + repo + "/" + webKind[n.Subject.Type] + "/" + path.Base(n.Subject.URL),
		})
	}
	return nil
}

var reviewVerbs = map[string]string{
	"APPROVED":          "approved",
	"CHANGES_REQUESTED": "requested changes on",
	"COMMENTED":         "commented on",
}

func (g *github) reviews(repo string, pr ghIssue, send func(tea.Msg)) error {
	var rs []struct {
		ID          int64     `json:"id"`
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submitted_at"`
		User        ghUser    `json:"user"`
	}
	if err := g.get(fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repo, pr.Number), &rs); err != nil {
		return err
	}
	for _, r := range rs {
		key := fmt.Sprint("review ", r.ID)
		verb, ok := reviewVerbs[r.State]
		if !ok || g.seen[key] || r.User.Login == g.me || !r.SubmittedAt.After(g.started) {
			continue
		}
		g.seen[key] = true
		send(Message{
			From: "GITHUB " + repo,
			Body: []string{fmt.Sprintf("%s %s #%d", r.User.Login, verb, pr.Number), pr.Title},
			URL:  pr.HTMLURL,
		})
	}
	return nil
}

func (g *github) merged(repo string, pr ghIssue, send func(tea.Msg)) error {
	at := pr.PullRequest.MergedAt
	key := fmt.Sprintf("merge %s#%d", repo, pr.Number)
	if at == nil || !at.After(g.started) || g.seen[key] {
		return nil
	}
	var full struct {
		MergedBy ghUser `json:"merged_by"`
	}
	if err := g.get(fmt.Sprintf("/repos/%s/pulls/%d", repo, pr.Number), &full); err != nil {
		return err
	}
	g.seen[key] = true
	if full.MergedBy.Login != g.me {
		send(Message{
			From: "GITHUB " + repo,
			Body: []string{fmt.Sprintf("%s merged #%d", full.MergedBy.Login, pr.Number), pr.Title},
			URL:  pr.HTMLURL,
		})
	}
	return nil
}

// search pages through all results. The search API stops at 1000 hits.
func (g *github) search(q string) ([]ghIssue, error) {
	var all []ghIssue
	for page := 1; page <= 10; page++ {
		var res struct {
			Items []ghIssue `json:"items"`
		}
		if err := g.get(fmt.Sprintf("/search/issues?per_page=100&page=%d&q=%s", page, url.QueryEscape(q)), &res); err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if len(res.Items) < 100 {
			break
		}
	}
	return all, nil
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func (g *github) get(endpoint string, v any) error {
	req, err := http.NewRequest("GET", g.api+endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
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
