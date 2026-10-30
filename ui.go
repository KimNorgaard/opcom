package main

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- look & feel knobs ------------------------------------------------------

type scheme struct {
	settled rgb // phosphor color once a char has "cooled"
	fresh   rgb // color of a char the instant it's written
	glow    rgb // faked bloom behind newly written chars
}

var schemes = map[string]scheme{
	// P1 phosphor, as on the green-screen VT100 variants.
	"green": {settled: rgb{51, 230, 70}, fresh: rgb{225, 255, 220}, glow: rgb{14, 66, 22}},
	// P3 phosphor (#FFB000), as on amber VT220s and Wyse terminals.
	"amber": {settled: rgb{255, 176, 0}, fresh: rgb{255, 240, 205}, glow: rgb{74, 46, 0}},
}

const (
	afterglow = 0.30 // seconds for foreground to decay to ~37% of "fresh"
	glowFade  = 0.12 // background glow fades faster than the text itself

	flickerAmt  = 0.03 // whole-screen brightness wobble per frame (±)
	shimmerAmt  = 0.06 // per-character brightness noise per frame (±/2)
	dipChance   = 0.01 // chance per frame of a brief brightness dip
	dipStrength = 0.85 // brightness multiplier during a dip

	initialPause = 600 * time.Millisecond
	backlog      = 400 // pending chars before typing speeds up
)

// ---- color helpers ----------------------------------------------------------

type rgb struct{ r, g, b float64 }

func mix(a, b rgb, k float64) rgb {
	return rgb{a.r + (b.r-a.r)*k, a.g + (b.g-a.g)*k, a.b + (b.b-a.b)*k}
}

func (c rgb) scale(f float64) rgb {
	cl := func(v float64) float64 { return math.Max(0, math.Min(255, v*f)) }
	return rgb{cl(c.r), cl(c.g), cl(c.b)}
}

func (c rgb) String() string {
	return fmt.Sprintf("%d;%d;%d", int(c.r), int(c.g), int(c.b))
}

// ---- realistic typing -------------------------------------------------------

// noise drives the visual jitter. Nothing here needs to be unpredictable.
func noise() float64 {
	return rand.Float64() // #nosec G404
}

// delayAfter returns how long to wait after printing c before the next char.
// Jittered base speed, pauses at punctuation and line ends, rare stalls
// (like a slow host or a busy serial line).
func delayAfter(c rune) time.Duration {
	ms := 28 + noise()*30 // 28–58 ms per char
	switch c {
	case '\n':
		ms += 250 + noise()*350
	case '.', ',', ':', ';':
		ms += 90 + noise()*80
	case ' ':
		ms += 15
	}
	if noise() < 0.03 {
		ms += 200 + noise()*500
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// ---- messages from sources --------------------------------------------------

// Message is a notification printed to the stream.
type Message struct {
	From string
	Body []string
	URL  string // optional, opened from the selection bar or by cmd+click
}

// linkStatus is sent by a source when its connection goes down or comes back.
type linkStatus struct {
	source string
	ok     bool
	detail string
}

// ---- bubbletea model --------------------------------------------------------

type tick time.Time

type model struct {
	sc    scheme
	text  []rune      // everything printed or queued for printing
	born  []time.Time // when each revealed character appeared
	n     int         // characters revealed so far
	next  time.Time   // earliest time the next character may appear
	w, h  int
	msgs  int
	down  map[string]bool // sources currently without a link
	links []link          // ordered by start
	sel   int             // selected link, -1 in live mode
}

// link is the rune range of a message block in text and the URL it opens.
type link struct {
	start, end int
	url        string
}

func newModel(sc scheme, banner string) model {
	return model{sc: sc, text: []rune(banner), next: time.Now().Add(initialPause), down: map[string]bool{}, sel: -1}
}

// safeURL returns u if it is an http(s) URL without control characters, so
// it can neither launch another handler through open nor break out of OSC 8.
func safeURL(u string) string {
	if !isHTTP(u) || strings.IndexFunc(u, unicode.IsControl) >= 0 {
		return ""
	}
	return u
}

func isHTTP(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https")
}

var openURL = func(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Run() // #nosec G204 -- u passed safeURL
}

type openFailed struct{ err error }

func open(u string) tea.Cmd {
	return func() tea.Msg {
		if err := openURL(u); err != nil {
			return openFailed{err}
		}
		return nil
	}
}

// selectable counts the links whose message has started typing.
func (m model) selectable() int {
	return sort.Search(len(m.links), func(i int) bool { return m.links[i].start >= m.n })
}

// linkAt returns the index of the link covering rune j, or -1.
func (m model) linkAt(j int) int {
	i := sort.Search(len(m.links), func(i int) bool { return m.links[i].end > j })
	if i < len(m.links) && m.links[i].start <= j {
		return i
	}
	return -1
}

func (m *model) print(s string) {
	for _, r := range strings.ToUpper(s) {
		// Source text is untrusted. Control characters would drive the terminal.
		if r == '\n' || unicode.IsPrint(r) {
			m.text = append(m.text, r)
		}
	}
}

func doTick() tea.Cmd {
	return tea.Tick(16*time.Millisecond, func(t time.Time) tea.Msg { return tick(t) })
}

func (m model) Init() tea.Cmd { return doTick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.sel = -1
		case "up", "k":
			if n := m.selectable(); m.sel < 0 && n > 0 {
				m.sel = n - 1
			} else if m.sel > 0 {
				m.sel--
			}
		case "down", "j":
			if m.sel >= 0 {
				m.sel++
				if m.sel >= m.selectable() {
					m.sel = -1
				}
			}
		case "enter":
			i := m.sel
			if i < 0 {
				i = m.selectable() - 1
			}
			if i >= 0 {
				return m, open(m.links[i].url)
			}
		}
	case openFailed:
		m.print(fmt.Sprintf("\n%%OPCOM-E-OPEN, %s\n", msg.err))
	case Message:
		m.msgs++
		var b strings.Builder
		fmt.Fprintf(&b, "\n%s INCOMING MESSAGE...\nFROM: %s\n", time.Now().Format("15:04"), msg.From)
		for _, l := range msg.Body {
			b.WriteString(l + "\n")
		}
		start := len(m.text) + 1 // past the blank separator line
		m.print(b.String())
		if u := safeURL(msg.URL); u != "" {
			m.links = append(m.links, link{start: start, end: len(m.text) - 1, url: u})
		}
	case linkStatus:
		if msg.ok {
			delete(m.down, msg.source)
			m.print(fmt.Sprintf("\n%%%s-S-CONNECT, LINK RESTORED\n", msg.source))
		} else {
			m.down[msg.source] = true
			m.print(fmt.Sprintf("\n%%%s-E-NOCARRIER, %s\n", msg.source, msg.detail))
		}
	case tick:
		now := time.Time(msg)
		if m.n < len(m.text) && !now.Before(m.next) {
			d := delayAfter(m.text[m.n])
			if len(m.text)-m.n > backlog {
				d /= 4
			}
			m.born = append(m.born, now)
			m.next = now.Add(d)
			m.n++
		}
		return m, doTick()
	}
	return m, nil
}

// lines splits the revealed text into [start, end) ranges, hard-wrapped one
// column short of the width so the cursor always fits. The final range is the
// line the cursor sits on.
//
// ponytail: rescans the whole history every frame, trim m.text if sessions
// ever run long enough for that to show up in CPU.
func (m model) lines() [][2]int {
	var lines [][2]int
	start := 0
	for i := 0; i < m.n; i++ {
		if m.text[i] == '\n' {
			lines = append(lines, [2]int{start, i})
			start = i + 1
		} else if i-start == m.w-1 {
			lines = append(lines, [2]int{start, i})
			start = i
		}
	}
	return append(lines, [2]int{start, m.n})
}

// window picks rows lines to show: the tail in live mode, or the selected
// message centered. It keeps no scroll state, so new output never moves a
// selection. bottom reports whether the cursor line is shown.
func (m model) window(lines [][2]int, rows int) (win [][2]int, bottom bool) {
	top := len(lines) - rows
	if m.sel >= 0 {
		l := m.links[m.sel]
		lineOf := func(p int) int { return sort.Search(len(lines), func(i int) bool { return lines[i][1] > p }) }
		first, last := lineOf(l.start), lineOf(l.end-1)
		top = first - (rows-(last-first+1))/2
	}
	top = max(0, min(top, len(lines)-rows))
	end := min(len(lines), top+rows)
	return lines[top:end], end == len(lines)
}

func (m model) View() string {
	if m.w < 20 || m.h < 3 {
		return ""
	}
	now := time.Now()

	// Flicker: small global wobble each frame, plus an occasional dip.
	frame := 1 - flickerAmt + noise()*flickerAmt*2
	if noise() < dipChance {
		frame *= dipStrength
	}

	rows := m.h - 2
	lines, bottom := m.window(m.lines(), rows)
	bar := m.sc.settled.scale(frame)

	var b strings.Builder
	for i, ln := range lines {
		inLink := -1
		for j := ln[0]; j < ln[1]; j++ {
			// OSC 8 links never stay open across a line end, and the id lets the
			// terminal treat a multi-line message as one link.
			if li := m.linkAt(j); li != inLink {
				if inLink >= 0 {
					b.WriteString("\x1b]8;;\x1b\\")
				}
				if li >= 0 {
					fmt.Fprintf(&b, "\x1b]8;id=opcom%d;%s\x1b\\", li, m.links[li].url)
				}
				inLink = li
			}
			if inLink >= 0 && inLink == m.sel {
				fmt.Fprintf(&b, "\x1b[38;2;8;8;8m\x1b[48;2;%sm%c", bar, m.text[j])
				continue
			}

			age := now.Sub(m.born[j]).Seconds()

			// Afterglow: fresh → settled color.
			k := math.Exp(-age / afterglow)
			shimmer := 1 + (noise()-0.5)*shimmerAmt
			fg := mix(m.sc.settled, m.sc.fresh, k).scale(frame * shimmer)

			// Faked bloom: dim colored background that fades out quickly.
			if g := math.Exp(-age / glowFade); g > 0.08 {
				fmt.Fprintf(&b, "\x1b[48;2;%sm", m.sc.glow.scale(g*frame))
			} else {
				b.WriteString("\x1b[49m")
			}
			fmt.Fprintf(&b, "\x1b[38;2;%sm%c", fg, m.text[j])
		}
		if inLink >= 0 && inLink == m.sel {
			b.WriteString(strings.Repeat(" ", max(0, m.w-1-(ln[1]-ln[0]))))
		}
		if inLink >= 0 {
			b.WriteString("\x1b]8;;\x1b\\")
		}
		b.WriteString("\x1b[49m")

		// Cursor stays solid while output is streaming, blinks when idle.
		if bottom && i == len(lines)-1 && (m.n < len(m.text) || now.UnixMilli()/530%2 == 0) {
			fmt.Fprintf(&b, "\x1b[38;2;%sm█", m.sc.settled.scale(frame))
		}
		b.WriteString("\x1b[0m\n")
	}
	b.WriteString(strings.Repeat("\n", rows-len(lines)))

	sep := strings.Repeat("─", m.w)
	if m.sel >= 0 {
		hint := " [↑↓] SELECT  [ENTER] OPEN  [ESC] LIVE "
		sep = hint + strings.Repeat("─", max(0, m.w-utf8.RuneCountInString(hint)))
	}
	fmt.Fprintf(&b, "\x1b[38;2;%sm%s\x1b[0m\n", m.sc.settled.scale(0.45*frame), sep)
	// Reverse video, like the host-writable status line on a VT320.
	fmt.Fprintf(&b, "\x1b[38;2;8;8;8m\x1b[48;2;%sm%s\x1b[0m", m.sc.settled.scale(frame), m.status(now))
	return b.String()
}

func (m model) status(now time.Time) string {
	state := "ONLINE"
	if len(m.down) > 0 {
		state = "NO CARRIER " + strings.Join(slices.Sorted(maps.Keys(m.down)), ",")
	}
	left := " OPCOM  " + state
	mid := fmt.Sprintf("MSG %04d", m.msgs)
	if m.sel >= 0 {
		mid = fmt.Sprintf("SEL %d/%d", m.sel+1, m.selectable())
	}
	right := strings.ToUpper(now.Format("02-Jan-2006 15:04")) + " "

	gap := m.w - len(left) - len(mid) - len(right)
	if gap < 2 {
		return (left + strings.Repeat(" ", m.w))[:m.w]
	}
	return left + strings.Repeat(" ", gap/2) + mid + strings.Repeat(" ", gap-gap/2) + right
}
