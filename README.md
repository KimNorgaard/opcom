# opcom

A personal notification stream for the terminal. opcom watches event sources
and types incoming messages out character by character, like an old VT100 or
VMS operator console.

```
14:32 INCOMING MESSAGE...
FROM: GITHUB EXAMPLE-ORG/WIDGET
ALEX1557 REQUESTS A REVIEW OF #123
FIX FLAKY TEST IN PARSER
█
────────────────────────────────────────────────────────────────
 OPCOM  ONLINE                 MSG 0004          27-OCT-2026 14:32
```

The screen simulates phosphor afterglow and flicker in green (P1) or amber
(P3). opcom is read-only. It shows messages but does not act on them.

## Build

Requires Go 1.27 or later.

```sh
just build        # or: go build -o opcom .
./opcom
```

## Keys

| Key            | Action                                                     |
|----------------|------------------------------------------------------------|
| `↑` / `k`      | Select the previous message that has a link                |
| `↓` / `j`      | Select the next one. Past the newest, return to live mode  |
| `Enter`        | Open the selected message, or the newest one in live mode  |
| `Esc`          | Return to live mode                                        |
| `q` / `ctrl+c` | Quit                                                       |

Selecting a message scrolls back to it, so older messages stay reachable.
GitHub and Jira messages are also terminal hyperlinks (OSC 8): cmd+click
opens them in terminals that support it, such as Ghostty, iTerm2 and kitty.

## Configuration

opcom reads `~/.config/opcom/config.json`. Use `-config <path>` to read another
file. Unknown fields are rejected.

```json
{
  "scheme": "amber",
  "fifo": "~/.config/opcom/notify",
  "jira": { "url": "https://jira.example.com" },
  "calendar": {
    "warn": ["10m", "1m"],
    "email": "you@example.com",
    "calendars": [
      { "name": "WORK", "gws": "primary" },
      { "name": "HOME", "url": "https://calendar.google.com/calendar/ical/.../basic.ics" }
    ]
  },
  "github": {
    "interval": "60s",
    "repos": [
      { "org": "acme" },
      { "repo": "acme/noisy-repo", "events": ["merged"] },
      { "repo": "example-org/widget" }
    ]
  }
}
```

| Key               | Default | Description                                   |
|-------------------|---------|-----------------------------------------------|
| `scheme`          | `green` | `green` or `amber`                            |
| `fifo`            |         | Path of the named pipe for local messages     |
| `github.interval` | `60s`   | Poll interval, as a Go duration               |
| `github.repos`    |         | Entries with either `repo` (`owner/name`) or `org` |
| `events`          | all     | Per entry. A `repo` entry overrides its `org` entry |
| `jira.url`        |         | Jira Server base URL, https only              |
| `jira.interval`   | `60s`   | Poll interval                                 |
| `jira.events`     | all     | `assigned`, `mentioned`                       |
| `calendar.refresh`| `10m`   | How often each feed is fetched. A calendar entry's own `refresh` overrides it. Google 429s secret iCal feeds polled too often |
| `calendar.warn`   | `10m`, `1m` | Lead times for meeting warnings           |
| `calendar.email`  |         | Your attendee address. Declined meetings are skipped |
| `calendar.calendars` |      | Entries with `name` and either an ICS `url` or a `gws` calendar ID |

## Sources

### GitHub

Authenticates with `GITHUB_TOKEN`, or with `gh auth token` when the variable
is unset.

| Event              | Reported when                                          |
|--------------------|--------------------------------------------------------|
| `review_requested` | A review is requested from you or one of your teams, on a PR not opened by a bot |
| `reviewed`         | Someone else submits a review on a PR you opened       |
| `merged`           | Someone else merges a PR you opened                    |
| `ci_failed`        | CI fails on an open PR you opened                      |
| `mentioned`        | You are @-mentioned in an issue or PR                  |

- PRs in archived repos and PRs opened by bots, such as renovate, are ignored.
- On startup, opcom prints a count of pending review requests and failing
  PRs instead of one message per PR.
- A PR that passes CI and then fails again is reported again.
- Reviews, merges and mentions are reported only if they happen while opcom runs.
- The message names the PR author as the requester, because the search API
  does not return who requested the review.
- Mentions come from the notifications API, which fine-grained tokens can't
  read. Use a classic token or `gh auth token`. Mentions of your teams are
  not reported.
- Each poll spends three or more GitHub search requests, depending on result
  count. The search API allows 30 per minute, so keep the interval at 30s or
  more.

### Local FIFO

When `fifo` is set, opcom creates the named pipe (mode 0600) if it doesn't
exist, and prints every non-empty line written to it as a message from
`LOCAL`.

```sh
make && echo "build done" 1<> ~/.config/opcom/notify
```

The `1<>` redirect opens the pipe read-write, so the write never blocks. When
opcom isn't running, the message is dropped. A plain `>` would block until
opcom reads the pipe. fish has no `<>` redirect, so call sh from fish:

```fish
sh -c 'echo "$1" 1<> ~/.config/opcom/notify' _ "build done"
```

Control characters are stripped from all messages before printing.

### Jira

Supports Jira Server and Data Center. Authenticates with a personal access
token in `JIRA_API_TOKEN`, sent as a Bearer token.

| Event       | Reported when                                              |
|-------------|------------------------------------------------------------|
| `assigned`  | An unresolved issue is assigned to you                     |
| `mentioned` | Someone else mentions you in a comment or a new issue      |

- On startup, opcom prints the number of assigned issues.
- An issue that is reassigned away and back is reported again.
- Mentions in descriptions are reported only for issues created while opcom
  runs, because Jira does not record when a description was edited.

### Calendar

Reads any number of calendars and warns before each meeting. Each calendar
is read in one of two ways:

- `url`: an ICS feed. For a Google calendar, open Settings, select the
  calendar and copy "Secret address in iCal format".
- `gws`: a Google calendar ID, such as `primary`, read through the
  [gws](https://github.com/googleworkspace/cli) CLI. Use this when a Workspace
  admin has disabled secret addresses. gws owns the OAuth login, so run
  `gws auth login` once. Declined meetings are detected without `email`.

- Recurring meetings, excluded dates, moved and cancelled instances, and time
  zones are handled.
- All-day events and meetings you declined (with `email` set) are skipped.
- Each lead time warns once. Starting opcom 30 seconds before a meeting gives
  one warning, not one per lead time.
- The secret URLs are credentials. opcom never prints them, and errors show
  only the host.
- A feed that fails to fetch keeps its last good copy, so warnings still fire.

### Link errors

When a source is unreachable, the status bar shows `NO CARRIER` with the failing
sources, such as `NO CARRIER CALENDAR`, and the stream prints a VMS-style error such as `%GITHUB-E-NOCARRIER, ...`.

## Development

```sh
just check   # format check, lint, gosec, govulncheck, race tests
just test
just fmt
```

## License

MIT. See [LICENSE](LICENSE).
