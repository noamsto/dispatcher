package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// LoadUsage is the line every `crew fleet load` grammar refusal prints.
const LoadUsage = "crew: fleet load [--json] [--budget N] [--add WEIGHT] [--own BRANCH]..."

// Exit codes of `crew fleet load`: fits, an error, and a refusal.
const (
	ExitFits    = 0
	ExitError   = 1
	ExitRefused = 3
)

const (
	defaultBudget = 8
	maxBudget     = 1000000
	// hotPct is the quota window level budget-gate.sh calls exhausted.
	hotPct = 95
	// maxBudgetAge and maxBudgetSkew bound a cache the predicates still trust.
	maxBudgetAge  = 7200
	maxTaskHead   = 64 << 10
	maxBudgetSkew = 300
)

// budgetEngines are always reported, so a missing or blind cache still names
// them as in budget.
var budgetEngines = []string{"claude", "codex", "cursor", "pi"}

// LoadOptions carry every outside read, so tests use temp dirs and stubs.
type LoadOptions struct {
	Getenv func(string) string
	// Registry overrides repos.RegistryPath: the repos whose released fleet holds
	// weigh as pending.
	Registry string
	Tmux     tmux.Client
	Git      git.Client
	Clock    clock.Clock
	// BudgetFile is engine-budget.json, the quota cache refresh-budget writes.
	BudgetFile string
	// Budget resolves fleetBudget from the dispatcher settings; unset when the
	// caller passes --budget.
	Budget func(ctx context.Context) (int, error)
	// Cmd runs a host probe (nproc, sysctl) from PATH and returns its stdout,
	// trimmed. An error is no answer.
	Cmd func(ctx context.Context, name string, args ...string) (string, error)
}

// LoadRequest is `crew fleet load`'s flags. Add, when positive, asks whether a
// dispatch of that weight fits; Own lists the branches of that dispatch.
type LoadRequest struct {
	JSON   bool
	Budget *int
	Add    int
	Own    []string
}

type Lead struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Name   string `json:"name"`
	Tier   string `json:"tier"`
	Weight int    `json:"weight"`
}

type Engine struct {
	InBudget     bool     `json:"in_budget"`
	HotWindows   []string `json:"hot_windows"`
	LimitReached bool     `json:"limit_reached"`
}

// LoadDoc is the machine's capacity: what the dispatch gate weighs. Free is
// null when fleetBudget is 0, which disables the check. Load1 and Cores are
// null when the host does not say; BudgetAgeS when the cache is unreadable.
type LoadDoc struct {
	Budget     int               `json:"budget"`
	Weight     int               `json:"weight"`
	Pending    int               `json:"pending"`
	Free       *int              `json:"free"`
	Leads      []Lead            `json:"leads"`
	Load1      *float64          `json:"load1"`
	Cores      *int              `json:"cores"`
	LoadOver   bool              `json:"load_over"`
	Engines    map[string]Engine `json:"engines"`
	BudgetAgeS *int              `json:"budget_age_s"`

	load1Text string // Load1 as the host printed it
}

const paneFormat = "#{pane_current_command}\x1f#{@crew_name}\x1f#{@crew_branch}\x1f#{@crew_role}\x1f#{pane_current_path}"

// pane is one `list-panes -a` row that is a lead's: stamped with a branch and
// no role, or role `lead`.
type pane struct {
	cmd, name, branch, path string
}

func leadPanes(ctx context.Context, c tmux.Client) []pane {
	var panes []pane
	for line := range strings.SplitSeq(tmux.Output(ctx, c, "list-panes", "-a", "-F", paneFormat), "\n") {
		f := strings.SplitN(line, "\x1f", 5)
		if len(f) < 5 || f[2] == "" || (f[3] != "" && f[3] != "lead") {
			continue
		}
		panes = append(panes, pane{cmd: f[0], name: f[1], branch: f[2], path: f[4]})
	}
	return panes
}

var tierLine = regexp.MustCompile(`(?m)^tier: (.*)$`)

// tierOf is the `tier:` header of the worktree's WORKER_TASK.md as a weight:
// trivial 1, deep 3, anything else, an unreadable header included, standard 2.
func tierOf(root string) (string, int) {
	// A worker owns this file: only a regular one is read (opening a FIFO
	// blocks), and only its head, so a symlink to /dev/zero cannot stall dispatch.
	path := root + "/WORKER_TASK.md"
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		if f, err := os.Open(path); err == nil {
			defer func() { _ = f.Close() }()
			data, _ := io.ReadAll(io.LimitReader(f, maxTaskHead))
			if m := tierLine.FindSubmatch(data); m != nil {
				switch string(m[1]) {
				case "trivial":
					return "trivial", 1
				case "deep":
					return "deep", 3
				}
			}
		}
	}
	return "standard", 2
}

// cleanText is worker-written text with control runes made "?", before it can
// forge a line or drive the terminal, as the roster table does.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// finishedBranches are the branches whose newest worker status row, in file
// order, is terminal. A body that is not an object has no state, which still
// replaces an earlier row. An unreadable bus finishes nothing.
func finishedBranches(common string) map[string]bool {
	events, _ := bus.ReadEventsTolerant(common + "/crew/events.jsonl")
	newest := map[string]string{}
	for _, e := range events {
		from, ok := strings.CutPrefix(e.From, "worker:")
		if e.Kind != bus.KindStatus || !ok {
			continue
		}
		if i := strings.LastIndex(from, "#"); i >= 0 {
			from = from[:i]
		}
		doc, _ := e.Doc.(map[string]any)
		body, _ := doc["body"].(map[string]any)
		state, _ := body["state"].(string)
		newest[from] = state
	}
	done := map[string]bool{}
	for b, s := range newest {
		done[b] = bus.State(s).Terminal()
	}
	return done
}

// scanLeads is every live lead on the machine and the branches that have a
// lead pane at all, engine alive or not. A lead is live when its pane still
// runs an engine in a worktree that still exists and its branch has not
// posted a terminal status; each repo's bus is read once. Role panes add
// nothing: the tier weight already includes them.
func scanLeads(ctx context.Context, o LoadOptions) (leads []Lead, owned map[string]bool) {
	owned = map[string]bool{}
	finished := map[string]map[string]bool{}
	for _, p := range leadPanes(ctx, o.Tmux) {
		owned[p.branch] = true
		if !roster.IsEngineCmd(p.cmd) {
			continue
		}
		if st, err := os.Stat(p.path); err != nil || !st.IsDir() {
			continue
		}
		root := git.Toplevel(ctx, o.Git, p.path)
		if root == "" {
			root = p.path
		}
		common := git.Output(ctx, o.Git, p.path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if common != "" {
			if _, ok := finished[common]; !ok {
				finished[common] = finishedBranches(common)
			}
			if finished[common][p.branch] {
				continue
			}
		}
		tier, w := tierOf(root)
		repo := "?"
		if common != "" {
			repo = filepath.Base(filepath.Dir(common))
		}
		name := p.name
		if name == "" {
			name = "?"
		}
		leads = append(leads, Lead{Repo: cleanText(repo), Branch: cleanText(p.branch), Name: cleanText(name), Tier: tier, Weight: w})
	}
	return leads, owned
}

var (
	loadRe  = regexp.MustCompile(`^[0-9]+([.][0-9]+)?$`)
	coresRe = regexp.MustCompile(`^[1-9][0-9]*$`)
)

func probe(ctx context.Context, o LoadOptions, name string, args ...string) string {
	run := o.Cmd
	if run == nil {
		run = runCmd
	}
	out, err := run(ctx, name, args...)
	if err != nil {
		return ""
	}
	return out
}

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// hostLoad is the 1-minute load average and the core count, from the sources
// the dispatch gate always used: $DISPATCH_LOADAVG_FILE else /proc/loadavg,
// else `sysctl -n vm.loadavg` (macOS); `nproc` from PATH, else `sysctl -n
// hw.ncpu`. Either unreadable is no answer, so the load check fails open. The
// load stays the text the host printed, which a refusal quotes.
func hostLoad(ctx context.Context, o LoadOptions) (load string, cores int, ok bool) {
	file := o.Getenv("DISPATCH_LOADAVG_FILE")
	if file == "" {
		file = "/proc/loadavg"
	}
	var raw string
	if f, err := os.Open(file); err == nil {
		fields := bufio.NewScanner(f)
		fields.Split(bufio.ScanWords)
		if fields.Scan() {
			raw = fields.Text()
		}
		_ = f.Close()
	} else if f := strings.Fields(probe(ctx, o, "sysctl", "-n", "vm.loadavg")); len(f) > 1 {
		raw = f[1]
	}
	nproc := probe(ctx, o, "nproc")
	if nproc == "" {
		nproc = probe(ctx, o, "sysctl", "-n", "hw.ncpu")
	}
	if !loadRe.MatchString(raw) || !coresRe.MatchString(nproc) {
		return "", 0, false
	}
	cores, _ = strconv.Atoi(nproc)
	return raw, cores, true
}

// ResolveBudget runs dispatch-config and reads fleetBudget the way the gate
// always did: a non-negative number, 8 when absent, floored and capped at a
// million so a float literal still caps.
func ResolveBudget(ctx context.Context, bin string, stderr io.Writer) (int, error) {
	out, err := configJSON(ctx, bin, stderr)
	if err != nil {
		return 0, err
	}
	var settings struct {
		FleetBudget *float64 `json:"fleetBudget"`
	}
	if err := json.Unmarshal(out, &settings); err != nil {
		return 0, fmt.Errorf("settings fleetBudget: %v", err)
	}
	return setting(settings.FleetBudget, defaultBudget, maxBudget), nil
}

// configJSON is dispatch-config's output: the merged dispatcher settings.
func configJSON(ctx context.Context, bin string, stderr io.Writer) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not resolve the dispatcher settings (%s: %w)", bin, err)
	}
	return out, nil
}

// setting is a numeric setting as a whole number: def when absent, floored and
// clamped to [0, ceiling], so a float literal still caps.
func setting(v *float64, def, ceiling int) int {
	n := float64(def)
	if v != nil {
		n = *v
	}
	return int(math.Min(math.Max(math.Floor(n), 0), float64(ceiling)))
}

func ConfigBin() string { return remote.ConfigBin() }

// budgetSummary is the quota cache per engine, with the predicates of
// budget-gate.sh: a window at 95% or more whose reset is null or still ahead,
// and the engine's own limit_reached. A cache that cannot tell (missing,
// unparsable, over 2 h old, over 300 s ahead, or no entry for the engine)
// reads as in budget, failing open as the launch gates do. The age is of the
// cache whenever it names one.
func budgetSummary(path string, now int64) (map[string]Engine, *int) {
	var cache struct {
		Fetched any            `json:"fetched_epoch"`
		Engines map[string]any `json:"engines"`
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cache)
	}
	fetched, haveEpoch := cache.Fetched.(float64)
	trusted := haveEpoch && fetched >= float64(now-maxBudgetAge) && fetched <= float64(now+maxBudgetSkew)
	names := append([]string(nil), budgetEngines...)
	for e := range cache.Engines {
		if !slices.Contains(names, e) {
			names = append(names, e)
		}
	}
	sort.Strings(names)
	engines := map[string]Engine{}
	for _, name := range names {
		e := Engine{InBudget: true, HotWindows: []string{}}
		if en, ok := cache.Engines[name].(map[string]any); ok && trusted && !onlyAnthropic(en) {
			e.HotWindows = hotWindows(en, now)
			e.LimitReached = limitReached(name, en, now)
			e.InBudget = len(e.HotWindows) == 0 && !e.LimitReached
		}
		engines[name] = e
	}
	if !haveEpoch {
		return engines, nil
	}
	age := max(int(float64(now)-fetched), 0)
	return engines, &age
}

// onlyAnthropic is pi's credit estimate alone, which is no quota window.
func onlyAnthropic(en map[string]any) bool {
	_, has := en["anthropic"]
	return has && len(en) == 1
}

func hotWindows(en map[string]any, now int64) []string {
	hot := []string{}
	windows, _ := en["windows"].(map[string]any)
	for key, w := range windows {
		win, _ := w.(map[string]any)
		pct, ok := win["used_pct"].(float64)
		if !ok || pct < hotPct {
			continue
		}
		if resets, ok := win["resets_at"].(float64); !ok || resets > float64(now) {
			hot = append(hot, key)
		}
	}
	sort.Strings(hot)
	return hot
}

// limitReached is `_budget_limit`: the engine's own limit with every window
// under 95%. Claude has none.
func limitReached(engine string, en map[string]any, now int64) bool {
	l, _ := en["limit_reached"].(map[string]any)
	switch engine {
	case "pi":
		reason, ok := l["reason"]
		return ok && reason != nil && reason != false
	case "cursor":
		if en["limit_reached"] == nil {
			return false
		}
		resets, ok := l["resets_at"].(float64)
		return !ok || resets > float64(now)
	case "codex":
		return l["rate_limit_reached_type"] != nil ||
			l["individual_remaining_percent"] == float64(0) ||
			l["spend_control_reached"] == true ||
			l["ordinary_usage_allowed"] == false
	}
	return false
}

// resolveBudget is the fleetBudget a call weighs against: the flag's, else the
// settings', else the default.
func resolveBudget(ctx context.Context, flag *int, o LoadOptions) (int, error) {
	switch {
	case flag != nil:
		return min(max(*flag, 0), maxBudget), nil
	case o.Budget != nil:
		return o.Budget(ctx)
	}
	return defaultBudget, nil
}

// collectLoad reads the machine's capacity against budget, and the branches
// that already have a lead pane.
func collectLoad(ctx context.Context, budget int, o LoadOptions) (LoadDoc, map[string]bool) {
	return measure(ctx, budget, o, scanFleet(o.Registry, o.Getenv, readBus, o.Clock.Now().UnixMilli()).released)
}

// measure is collectLoad over a fleet scan already read. A fleet hold released
// within the pending window weighs as pending until its branch has a lead
// pane: its dispatch is still on the way, and a second release in that gap
// must not count the capacity as free.
func measure(ctx context.Context, budget int, o LoadOptions, released []releasedHold) (LoadDoc, map[string]bool) {
	leads, owned := scanLeads(ctx, o)
	if leads == nil {
		leads = []Lead{}
	}
	sort.SliceStable(leads, func(i, j int) bool { return leads[i].Weight > leads[j].Weight })
	doc := LoadDoc{Budget: budget, Leads: leads}
	for _, l := range leads {
		doc.Weight += l.Weight
	}
	for _, r := range released {
		if !owned[r.branch] {
			doc.Pending += r.weight
		}
	}
	if budget > 0 {
		free := max(budget-doc.Weight-doc.Pending, 0)
		doc.Free = &free
	}
	if text, cores, ok := hostLoad(ctx, o); ok {
		load, _ := strconv.ParseFloat(text, 64)
		doc.Load1, doc.Cores, doc.load1Text = &load, &cores, text
		doc.LoadOver = load > float64(cores)
	}
	doc.Engines, doc.BudgetAgeS = budgetSummary(o.BudgetFile, o.Clock.Now().Unix())
	return doc, owned
}

// Load reads the machine's capacity and, when req.Add is positive, judges a
// dispatch of that weight against it: exit 0 fits, 3 refuses (the reason on
// stdout, or stderr beside the JSON document), 1 is an error. fleetBudget 0
// disables both checks, and a dispatch onto a branch that already has a lead
// pane is no new lead.
func Load(ctx context.Context, req LoadRequest, stdout, stderr io.Writer, o LoadOptions) int {
	budget, err := resolveBudget(ctx, req.Budget, o)
	if err != nil {
		say(stderr, "crew: fleet load: %s\n", oneLine(err))
		return ExitError
	}
	doc, owned := collectLoad(ctx, budget, o)

	why := ""
	if req.Add > 0 && budget > 0 && !ownsAny(owned, req.Own) {
		why = refusal(doc, req.Add)
	}

	if req.JSON {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(doc); err != nil {
			say(stderr, "crew: fleet load: %s\n", oneLine(err))
			return ExitError
		}
		if why != "" {
			say(stderr, "%s\n", why)
		}
	} else if why != "" {
		say(stdout, "%s\n", why)
	} else {
		writeLoad(stdout, doc)
	}
	if why != "" {
		return ExitRefused
	}
	return ExitFits
}

func ownsAny(owned map[string]bool, branches []string) bool {
	for _, b := range branches {
		if owned[b] {
			return true
		}
	}
	return false
}

// refusal is why a dispatch of weight add does not fit, "" when it does:
// the live lead weight against fleetBudget first, then the load average.
func refusal(d LoadDoc, add int) string {
	if d.Weight+d.Pending+add > d.Budget {
		who := make([]string, 0, 3)
		for _, l := range d.Leads[:min(len(d.Leads), 3)] {
			who = append(who, fmt.Sprintf("%s (%s, %s)", l.Name, l.Repo, l.Tier))
		}
		pending := ""
		if d.Pending > 0 {
			pending = fmt.Sprintf(" + pending %d", d.Pending)
		}
		why := fmt.Sprintf("live lead weight %d%s + this %sdispatch (%d) exceeds fleetBudget %d", d.Weight, pending, tierWord(add), add, d.Budget)
		if len(who) > 0 {
			why += " — heaviest: " + strings.Join(who, ", ")
		}
		return why
	}
	if d.LoadOver {
		return fmt.Sprintf("1-minute load average %s is above the %d cores", d.load1Text, *d.Cores)
	}
	return ""
}

func tierWord(add int) string {
	switch add {
	case 1:
		return "trivial "
	case 2:
		return "standard "
	case 3:
		return "deep "
	}
	return ""
}

func writeLoad(w io.Writer, d LoadDoc) {
	if d.Free == nil {
		say(w, "fleetBudget 0 (checks off)  live lead weight %d\n", d.Weight)
	} else {
		say(w, "fleetBudget %d  live lead weight %d  pending %d  free %d\n", d.Budget, d.Weight, d.Pending, *d.Free)
	}
	if d.Load1 != nil {
		say(w, "load %.2f on %d cores\n", *d.Load1, *d.Cores)
	}
	for _, l := range d.Leads {
		say(w, "  %-14s %-8s %-9s weight %d  %s\n", l.Name, l.Tier, l.Repo, l.Weight, l.Branch)
	}
}
