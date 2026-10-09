package stall

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/frame"
)

// frameDet is the frame-driven detectors' state across ticks: the *_at stamps
// are the tick an episode was posted on (0 = none), the rest are D1/D1b hit
// counters and D2/D7's running evidence.
type frameDet struct {
	d0At int64

	d1At, d1Hits int64
	d1Kind       string // what was actually posted, for the mid-episode flip

	d1bAt, d1bHits int64

	d2At, d2Since  int64
	d2Tok, d2Clock string // strings, never numbers: a parse failure is no new branch
	d2Moved        bool
	d7At, d7Hits   int64
	d7Tok0         int64
	d3At           int64
}

// space is glibc's [[:space:]] in a UTF-8 locale, which RE2's own class is not.
const space = `\t\n\v\f\r \x{1680}\x{2000}-\x{2006}\x{2008}-\x{200A}\x{2028}\x{2029}\x{205F}\x{3000}`

var (
	reSentinel = regexp.MustCompile(`<｜[^｜>]{1,40}｜>|<\|(im_start|im_end|endoftext|eot_id|start_header_id|end_header_id)\|>`)

	reRunawayReset  = regexp.MustCompile(`^[` + space + `]*⏺`)
	reRunawayCall   = regexp.MustCompile(`^[` + space + `]*⏺[` + space + `]+[A-Za-z_]+\(`)
	reRunawayQuote  = regexp.MustCompile(`^[` + space + `]*(>|❯)`)
	reRunawayResult = regexp.MustCompile(`^[` + space + `]*⎿`)
	reRunawayTool   = regexp.MustCompile(`^[` + space + `]*Tool output`)
	reBlank         = regexp.MustCompile(`^[` + space + `]*$`)

	reOutTokens = regexp.MustCompile(`↓ ?[0-9.]+[kM]?`)
	reNumPrefix = regexp.MustCompile(`^[0-9]*\.?[0-9]*`)

	// sed -E 's/^[^(]*\(([^·]*)·.*/\1/'
	reMeterClock = regexp.MustCompile(`^[^(]*\(([^·]*)·`)
	// The suffix of sed -E 's/.*↓[[:space:]]*([0-9.]+k?)[[:space:]]tokens.*/\1/';
	// the leading greedy `.*` takes the last ↓ that fits.
	reMeterTokens = regexp.MustCompile(`^↓[` + space + `]*([0-9.]+k?)[` + space + `]tokens`)

	rePermOSC    = regexp.MustCompile(`\x1b\][^\x07\x1b\n]*(\x07|\x1b\\)?`)
	rePermCSI    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	rePermEsc    = regexp.MustCompile(`\x1b[@-Z\\-_]`)
	rePermHeader = regexp.MustCompile(`^[` + space + `]*[A-Za-z][A-Za-z ]+ · from the `)
	rePermTool   = regexp.MustCompile(`[` + space + `]+· from the .*`)
	reLeadSpace  = regexp.MustCompile(`^[` + space + `]+`)
	reTrimSpace  = regexp.MustCompile(`^[` + space + `]+|[` + space + `]+$`)
	reRunSpace   = regexp.MustCompile(`[` + space + `]+`)
)

// frameDetect runs D1, D2, D1b, D7, D3 and D0 on this tick's frame.
func (w *watch) frameDetect() error {
	for _, d := range []func() error{w.d1, w.d2, w.d1b, w.d7, w.d3, w.d0} {
		if err := d(); err != nil {
			return err
		}
	}
	return nil
}

// d1 is the interactive-prompt detector. Presence, not transition: the
// workspace-trust frame is on screen from the worker's first sample, so an
// "appeared" conjunct could never fire on the only frame with production
// occurrences. quota: is a content discriminator on the same geometry, so one
// hit counter covers both; d1Kind remembers which was posted so a mid-episode
// flip reclassifies instead of staying mislabeled. A tool-permission dialog is
// checked first: its footer is not an `Enter to select` frame and must not be
// gated on IsPrompt's veto.
func (w *watch) d1() error {
	fd := &w.fd
	kind := ""
	if !w.suppressed && w.cfg.sigPrompt {
		switch {
		case frame.IsPermissionPrompt(w.cfg.engine, w.text):
			kind = "permission"
		case frame.IsPrompt(w.cfg.engine, w.text) && frame.MeterLine(w.text) == "":
			kind = "prompt"
			if frame.IsQuotaPrompt(w.text) {
				kind = "quota"
			}
		}
	}
	if kind == "" {
		if fd.d1At != 0 {
			if err := w.clearPrompt(); err != nil {
				return err
			}
			fd.d1At = 0
		}
		fd.d1Hits = 0
		fd.d1Kind = ""
		return nil
	}
	if fd.d1At != 0 && kind != fd.d1Kind {
		if err := w.clearPrompt(); err != nil {
			return err
		}
		fd.d1At = 0
		fd.d1Hits = 0
	}
	fd.d1Hits++
	if fd.d1Hits < 2 || fd.d1At != 0 {
		return nil
	}
	prefix, detail := "prompt:", "prompt: interactive prompt in pane "+w.cfg.pane+" — worker is waiting on input nobody can give"
	switch kind {
	case "permission":
		detail = permissionDetail(w.text, w.cfg.pane)
	case "quota":
		prefix = "quota:"
		detail = "quota: quota exhausted — worker parked on the rate-limit prompt in pane " + w.cfg.pane +
			"; do not re-dispatch — Esc dismisses it, resume continues from intact context on the next window"
	}
	posted, err := w.postBlocked(prefix, detail)
	if err != nil || !posted {
		return err
	}
	fd.d1At, fd.d1Kind = w.now, kind
	return nil
}

func (w *watch) clearPrompt() error {
	if err := w.postClear("prompt:"); err != nil {
		return err
	}
	return w.postClear("quota:")
}

// d2 is the dead-turn detector: the meter clock advancing while its token
// string stays static.
func (w *watch) d2() error {
	fd := &w.fd
	if w.suppressed || !w.cfg.sigMeter || w.cfg.roleMode {
		return nil
	}
	m := frame.MeterLine(w.text)
	if m == "" || frame.HasSubrow(w.text) {
		// A live subagent row is an unconditional veto: a healthy deep worker
		// in a subagent batch reproduces D2's exact signature — meter present,
		// clock rising, token string static — for minutes at a stretch. The
		// cost is a stated blind spot: a turn that dies with a row still
		// painted is invisible to D2.
		if err := w.clearTurnStall(); err != nil {
			return err
		}
		fd.d2Since, fd.d2Tok, fd.d2Clock, fd.d2Moved = 0, "", "", false
		return nil
	}
	clock, tok := meterClock(m), meterTokens(m)
	if fd.d2Since == 0 || tok != fd.d2Tok {
		if err := w.clearTurnStall(); err != nil {
			return err
		}
		fd.d2Tok, fd.d2Clock, fd.d2Since, fd.d2Moved = tok, clock, w.now, false
	} else if clock != fd.d2Clock {
		// A rising clock proves the capture is a live frame. A static clock
		// means a frozen renderer or copy-mode scrollback — evidence we cannot
		// trust, so the rule stays silent.
		fd.d2Clock, fd.d2Moved = clock, true
	}
	// Any status event from the worker in the window is a sign of life, with
	// the same one-second slack as the run start: the window opens on a
	// truncated clock and the event carries ms.
	if w.bus.source != "watchdog" && w.bus.ts >= (fd.d2Since-1)*1000 {
		fd.d2Since, fd.d2Moved = w.now, false
	}
	if fd.d2At != 0 || !fd.d2Moved || w.now-fd.d2Since < w.cfg.idle {
		return nil
	}
	posted, err := w.postBlocked("turn-stall:", "turn-stall: token count static at "+fd.d2Tok+" for "+
		strconv.FormatInt(w.now-fd.d2Since, 10)+"s while the pane clock advanced")
	if err != nil || !posted {
		return err
	}
	fd.d2At = w.now
	return nil
}

func (w *watch) clearTurnStall() error {
	if w.fd.d2At == 0 {
		return nil
	}
	if err := w.postClear("turn-stall:"); err != nil {
		return err
	}
	w.fd.d2At = 0
	return nil
}

// meterClock and meterTokens are the two sed substitutions over the meter
// line; a line the pattern does not match comes back whole.
func meterClock(m string) string {
	if sub := reMeterClock.FindStringSubmatch(m); sub != nil {
		return sub[1]
	}
	return m
}

func meterTokens(m string) string {
	for i := strings.LastIndex(m, "↓"); i >= 0; i = strings.LastIndex(m[:i], "↓") {
		if sub := reMeterTokens.FindStringSubmatch(m[i:]); sub != nil {
			return sub[1]
		}
	}
	return m
}

// d1b is the quota-refusal detector: claude's session limit and cursor's
// monthly usage limit. A second quota: frame and its own detector because it
// satisfies neither D1 (no option-select geometry) nor D2 (it matches no
// meter, so D2 reads it as "no active turn"); left alone it falls through to
// D3 quiet:, which escalates. Each engine enables only its own signature.
func (w *watch) d1b() error {
	fd := &w.fd
	kind := ""
	if !w.suppressed {
		switch {
		case w.cfg.sigSessionLimit && frame.IsQuotaSessionLimit(w.text):
			kind = "session"
		case w.cfg.sigCursorLimit && frame.IsQuotaCursorLimit(w.text):
			kind = "cursor"
		}
	}
	if kind == "" {
		if fd.d1bAt != 0 {
			if err := w.postClear("quota:"); err != nil {
				return err
			}
			fd.d1bAt = 0
		}
		fd.d1bHits = 0
		return nil
	}
	fd.d1bHits++
	if fd.d1bHits < 2 || fd.d1bAt != 0 {
		return nil
	}
	detail := "quota: session limit — do not re-dispatch; wait for the reset shown in pane " + w.cfg.pane +
		", or a human can run /low-priority there (spends weekly budget) — Esc/Enter will not submit a queued prompt while the limit holds"
	if kind == "cursor" {
		detail = "quota: cursor monthly usage limit — do not re-dispatch; the limit resets on the Cursor billing cycle, not on a retry, so pane " +
			w.cfg.pane + " stays parked until then"
	}
	posted, err := w.postBlocked("quota:", detail)
	if err != nil || !posted {
		return err
	}
	fd.d1bAt = w.now
	return nil
}

// d7 is the runaway-output detector, the opposite of D2/D3: a degenerated turn
// repaints constantly and its token count climbs, so it reads as a busy worker
// forever. A leaked model sentinel in assistant prose is the engine-independent
// tell; it must persist --runaway-hits samples and the output-token count must
// have grown by --runaway-tokens since the first hit, so a transient mention
// cannot post. Never escalates: the turn is unrecoverable but the worktree
// usually isn't.
func (w *watch) d7() error {
	fd := &w.fd
	if !w.suppressed && w.cfg.sigRunaway && !w.cfg.roleMode && reSentinel.MatchString(runawayProse(w.text)) {
		tok, ok := outTokens(w.text)
		if !ok {
			// A sentinel with no readable token count neither counts nor
			// resets the episode.
			return nil
		}
		if fd.d7Hits == 0 || tok < fd.d7Tok0 {
			fd.d7Tok0 = tok
		}
		fd.d7Hits++
		grew := tok - fd.d7Tok0
		if fd.d7At != 0 || fd.d7Hits < w.cfg.runawayHits || grew < w.cfg.runawayTokens {
			return nil
		}
		posted, err := w.postBlocked("runaway:", "runaway: leaked model sentinel in pane "+w.cfg.pane+" for "+
			strconv.FormatInt(fd.d7Hits, 10)+" samples while output tokens grew by "+strconv.FormatInt(grew, 10)+
			" — the turn is degenerate; verify the pane, then kill and re-dispatch (the worktree usually survives)")
		if err != nil || !posted {
			return err
		}
		fd.d7At = w.now
		return nil
	}
	if fd.d7At != 0 {
		if err := w.postClear("runaway:"); err != nil {
			return err
		}
		fd.d7At = 0
	}
	fd.d7Hits, fd.d7Tok0 = 0, 0
	return nil
}

// runawayProse is the bottom 60 lines of the frame with tool output removed,
// so a worker legitimately reading or editing text that contains a sentinel
// (a fixture, the detector itself) never matches. claude: a `⎿` result row and
// its deeper-indented continuation, until the next `⏺` row; pi: a `Tool
// output` row until the next blank line. A `⏺` row restarts the buffer.
func runawayProse(text string) string {
	lines := strings.Split(text, "\n")
	lines = lines[max(len(lines)-60, 0):]
	var buf []string
	skip := 0
	for _, l := range lines {
		if reRunawayReset.MatchString(l) {
			skip, buf = 0, buf[:0]
		}
		switch {
		case reRunawayCall.MatchString(l), reRunawayQuote.MatchString(l):
			continue
		case reRunawayResult.MatchString(l):
			skip = 1
			continue
		case reRunawayTool.MatchString(l):
			skip = 2
			continue
		case skip == 1 && (reBlank.MatchString(l) || strings.HasPrefix(l, "     ")):
			continue
		case skip == 2 && (reBlank.MatchString(l) || strings.HasPrefix(l, "  ")):
			continue
		}
		skip = 0
		buf = append(buf, l)
	}
	return strings.Join(buf, "\n")
}

// outTokens is the output-token count as an integer: the last `↓ N[kM]` in the
// bottom 12 lines (claude's live meter, pi's footer). The number goes through
// awk's rules: its numeric prefix, times the unit, truncated.
func outTokens(text string) (int64, bool) {
	lines := strings.Split(text, "\n")
	found := reOutTokens.FindAllString(strings.Join(lines[max(len(lines)-12, 0):], "\n"), -1)
	if len(found) == 0 {
		return 0, false
	}
	n := strings.TrimPrefix(found[len(found)-1], "↓")
	n = strings.TrimPrefix(n, " ")
	mult := 1.0
	switch {
	case strings.HasSuffix(n, "k"):
		mult = 1000
	case strings.HasSuffix(n, "M"):
		mult = 1000000
	}
	n = strings.TrimRight(n, "kM")
	f, _ := strconv.ParseFloat("0"+reNumPrefix.FindString(n), 64)
	return int64(f * mult), true
}

// d3 is the quiet-pane detector. Byte-identity, not "no meter": a healthy
// claude pane repaints its spinner every second, so a working worker can never
// satisfy D3 even if every claude signature rots to garbage. This is the
// failsafe for signature rot and the only steady-state coverage codex and
// cursor get.
func (w *watch) d3() error {
	fd := &w.fd
	if !w.suppressed && !w.bgwait && fd.d3At == 0 && w.quietFor >= w.cfg.idle && !w.cfg.roleMode &&
		(w.bus.source == "watchdog" || w.bus.ts < w.lastChange*1000) {
		posted, err := w.postBlocked("quiet:", "quiet: pane unchanged for "+strconv.FormatInt(w.quietFor, 10)+"s")
		if err != nil {
			return err
		}
		if posted {
			fd.d3At = w.now
		}
	}
	if fd.d3At != 0 && w.quietFor < w.cfg.idle {
		if err := w.postClear("quiet:"); err != nil {
			return err
		}
		fd.d3At = 0
	}
	return nil
}

// d0 is startup silence. Classify before judging: a static pane that is a
// prompt belongs to D1 and D0 says nothing about it. The detail carries no
// diagnosis; the old `(suspected startup/indexing hang)` was wrong on 3/3
// measured workers, and an invented cause reads as corroboration.
func (w *watch) d0() error {
	fd := &w.fd
	if w.suppressed || w.bgwait || fd.d0At != 0 || w.cfg.roleMode ||
		w.now-w.start >= w.cfg.window || w.quietFor < w.cfg.stall {
		return nil
	}
	if w.bus.state != "" && w.bus.state != "working" {
		return nil
	}
	if w.cfg.sigPrompt && (frame.IsPermissionPrompt(w.cfg.engine, w.text) || frame.IsPrompt(w.cfg.engine, w.text)) {
		return nil
	}
	posted, err := w.postBlocked("stalled:", "stalled: no output for "+strconv.FormatInt(w.cfg.stall, 10)+"s")
	if err != nil || !posted {
		return err
	}
	fd.d0At = w.now
	return nil
}

// escalate is the only path to `failed`, and a second, later, independent
// evidence check, not a timer. prompt: and quota: are exempt: a frame still on
// screen means nobody answered (or the window has not reset), and escalating
// either would launder an intact worker into a `failed` that invites exactly
// the re-dispatch quota: exists to prevent. stalled: is exempt too — it
// catches the classifier's misses, so it stays recoverable; a dead pane
// reaches `failed` through quiet:. The live bus, not the in-process timer,
// decides: a second watchdog on the branch (INV-W3 allows two) may own the
// open episode now. quiet: additionally requires the pane's engine to be gone.
func (w *watch) escalate() error {
	fd := &w.fd
	if w.cfg.roleMode {
		return nil
	}
	if fd.d2At != 0 && w.now-fd.d2At >= w.cfg.dead {
		if err := w.refresh(); err != nil {
			return err
		}
		if !holdsEpisode(w.bus.detail) {
			if err := w.post("failed", "dead: turn-stall: unchanged for "+strconv.FormatInt(w.now-fd.d2At, 10)+"s"); err != nil {
				return err
			}
			return exitCode(0)
		}
	}
	if fd.d3At != 0 && w.now-fd.d3At >= w.cfg.dead {
		if err := w.refresh(); err != nil {
			return err
		}
		if !holdsEpisode(w.bus.detail) && !w.engineAlive() {
			if err := w.post("failed", "dead: quiet: unchanged for "+strconv.FormatInt(w.now-fd.d3At, 10)+"s"); err != nil {
				return err
			}
			return exitCode(0)
		}
	}
	return nil
}

// holdsEpisode is an open prompt:, quota: or runaway: episode, which never
// escalates.
func holdsEpisode(detail string) bool {
	return strings.HasPrefix(detail, "prompt:") || strings.HasPrefix(detail, "quota:") || strings.HasPrefix(detail, "runaway:")
}

// permissionDetail is the human-relay payload for a detected tool-permission
// dialog. It is pane-scraped and therefore attacker-influenceable, so it is
// stripped before it reaches the bus: ESC/CSI sequences and control characters
// removed, folded to one line. The trailing ` — pane <id>` is never truncated
// (recovery needs it), so only the `permission — <tool>: <request>` prefix is
// capped, keeping the whole detail at 160 characters. A frame whose tool or
// request cannot be parsed still fires, with `(unparsed)`. The header is the
// last `· from the …` line in the capture, not the first: a prior dialog
// scrolled into the transcript must not make the relay report its stale
// tool/request while the live dialog is parked at the bottom.
func permissionDetail(text, pane string) string {
	clean := rePermOSC.ReplaceAllString(text, "")
	clean = rePermCSI.ReplaceAllString(clean, "")
	clean = rePermEsc.ReplaceAllString(clean, "")
	clean = string(stripControl([]byte(clean)))

	var tool, req string
	have := false
	for _, l := range strings.Split(clean, "\n") {
		switch {
		case rePermHeader.MatchString(l):
			tool = rePermTool.ReplaceAllString(reLeadSpace.ReplaceAllString(l, ""), "")
			req, have = "", true
		case have && req == "" && !reBlank.MatchString(l):
			req = reTrimSpace.ReplaceAllString(l, "")
		}
	}
	body := "prompt: permission — (unparsed)"
	if have && req != "" {
		body = "prompt: permission — " + tool + ": " + req
	}
	body = reRunSpace.ReplaceAllString(strings.NewReplacer("\n", " ", "\t", " ").Replace(body), " ")

	suffix := " — pane " + pane
	limit := max(160-len([]rune(suffix)), 0)
	if r := []rune(body); len(r) > limit {
		body = string(r[:limit])
	}
	return body + suffix
}

// stripControl is `tr -d '\000-\010\013-\037\177'`: it keeps tab and newline.
func stripControl(b []byte) []byte {
	out := b[:0]
	for _, c := range b {
		if c <= 8 || (c >= 11 && c <= 31) || c == 127 {
			continue
		}
		out = append(out, c)
	}
	return out
}
