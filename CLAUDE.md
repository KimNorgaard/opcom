# opcom

Personal notification stream in a full-screen terminal. Events from sources are
typed out like an old VT100/VMS terminal, Fallout-terminal flavour. Read-only:
no reacting to messages yet.

## Commands

- `just check`: CI gate (fmt-check, golangci-lint, gosec, govulncheck, race tests). Must pass.
- `just fmt`: gofumpt + goimports via golangci-lint.
- `just run -config ./config.example.json`
- Config default: `~/.config/opcom/config.json`. See `config.example.json`.

## Layout

- `main.go`: config loading (JSON, unknown fields rejected), source wiring, boot banner.
- `ui.go`: bubbletea model. Colour schemes, typewriter, phosphor afterglow, flicker, status bar.
- `github.go`: GitHub source. `github_test.go` drives it against `httptest` via `g.api`.
- `fifo.go`: local named-pipe source. One line is one message.
- `jira.go`: Jira Server source. `calendar.go`: ICS calendar source.

## Sources

- A source is a constructor that validates config and fails before the UI starts,
  plus `run(send func(tea.Msg))` started as a goroutine. No interface until a
  second source needs one.
- Sources send `Message` (body in natural case, the UI uppercases) and
  `linkStatus` only on up/down transitions.
- Link errors print VMS-style: `%SOURCE-E-NOCARRIER, detail`.

## GitHub source

- Token from `GITHUB_TOKEN`, else `gh auth token`.
- Three searches per poll across all of GitHub, filtered locally by repo/org.
  Keeps search rate limit (30/min) independent of repo count. Don't switch to
  per-repo queries or `repo:` qualifiers.
- Watch searches add `archived:false`, and `snapshot` drops bot-authored PRs
  (`user.type == "Bot"`). Renovate team requests were ~85% of all hits.
- Review requests (`review-requested:@me`, includes teams) and CI failures
  (`status:failure`) are `watch` searches: set-diffed per poll, first poll
  prints only a count. Team requests can be hundreds of hits, so `search`
  paginates.
- Mentions use the notifications API (`reason == "mention"`), because search
  can't say when a mention happened. Fine-grained tokens can't read it.
- Reviews, merges and mentions are reported only if they happened after
  startup; `seen` dedupes across overlapping poll windows.
- A `repo` entry overrides its `org` entry's events.

## Jira source

- Server/DC REST v2 with a PAT as `Authorization: Bearer`. Not Jira Cloud.
- Mentions are wiki markup `[~<name>]`, where `<name>` is `/myself`'s `name`.
  Find candidates with `text ~ "<name>"`, then confirm in comment bodies.
- JQL uses relative dates (`updated >= -Nm`). Absolute dates are parsed in the
  user's Jira profile timezone.
- Timestamps have no colon in the offset. Use `jiraTime`.

## Calendar source

- `emersion/go-ical` parses and expands RRULE/EXDATE/RDATE with TZID.
  RECURRENCE-ID overrides and all-day detection are handled in `expand`.
- ICS URLs are secrets. Never include them in errors or messages.
- `gws.go` feeds use the gws CLI for Workspace calendars where secret ICS
  addresses are disabled. `singleEvents` makes Google expand recurrences.
  gws writes log lines to stderr, so only stdout is parsed.
- `check` warns once per stage: the smallest lead time reached.

## FIFO source

- Opened `O_RDWR` so reads never hit EOF between writers (works on Linux and macOS).
- Writers should use `1<>`, which never blocks. `>` blocks while opcom isn't running.

## UI

- `Message.URL` is validated by `safeURL` in the model (http(s), no control
  characters) before it reaches `open` or an OSC 8 sequence.
- OSC 8 links are closed at every line end and carry an `id`, so a wrapped
  message stays one link.
- `window` is stateless: the selected message is centered each frame, so new
  output never moves a selection.
- `model.print` strips control characters. Source text is untrusted and goes
  straight to the terminal.
- Schemes: `green` (P1) and `amber` (P3, #FFB000). Keep colours period-accurate.
- `View` must emit exactly terminal height lines: stream, `─` separator, reverse-video status bar.
- Only `tick` re-arms `doTick()`. Returning it from other messages multiplies tick loops.

## Conventions

- Lint config forbids nested anonymous structs: use named types (`ghUser`, `ghPR`).
- Suppress gosec only with `#nosec <rule>` and a reason, at a single site.
- Deliberate shortcuts with a known ceiling get a `ponytail:` comment.
