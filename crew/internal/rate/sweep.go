// The sweep: fold the repo's bus into run records, reconcile each run's PR
// against GitHub, and merge the result forward into the global ratings store.
// It is the `rate)` arm's default mode, ported op-for-op — same order of
// operations, same store rows, same two lock windows with the network between
// them, same one line on stderr per failure mode and nothing on stdout.
package rate

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/classes"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

//go:embed records.jq
var recordsProgram string

//go:embed plan.jq
var planProgram string

//go:embed view.jq
var viewProgram string

//go:embed actions.jq
var actionsProgram string

//go:embed threads.jq
var threadsProgram string

//go:embed merge.jq
var mergeProgram string

const (
	// busy is the lock's only refusal line: the autosweep spawner
	// absorbs it, an interactive `crew rate` takes it as its exit status.
	busy = "crew: ratings store busy"
	// burnWarn is the one warning printed before the store is touched. Without
	// a resolver the burn table is empty, which per-row looks exactly like a
	// legitimately unclassed model — so it says so once.
	burnWarn = "crew rate: could not resolve settings (dispatch-config unavailable) — " +
		"every model will read as unclassed; set DISPATCH_CONFIG_BIN or install dispatch-config beside crew"
	// threadsQuery is the arm's GraphQL query, byte for byte: the stub gh in
	// the bats suite matches on it.
	threadsQuery = "query($owner:String!,$name:String!,$number:Int!){" +
		"repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{isResolved}}}}}"
)

// sweep is one run of the arm's default mode, holding the paths it resolved
// and the seams it reads the world through.
type sweep struct {
	paths  bus.Paths
	cwd    string
	stderr io.Writer
	o      Options
	pid    string
	store  string
	lockd  string
	ghRepo string
}

// clock is jqrun's `now`, read at the moment the fold runs. The arm ran every
// fold in its own jq process, so `now` was re-read each time: records and plan
// before the network, merge after it. A value frozen at sweep start would date
// the merged row to the start while readers fold the store last-wins by
// max_by(.swept_at), discarding an overlapping sweep's fresher row as stale.
func (s *sweep) clock() float64 { return clockSeconds(s.o) }

// runSweep is the arm, in the arm's order.
func runSweep(paths bus.Paths, cwd string, stderr io.Writer, o Options) int {
	// `[ -f "$log" ] || exit 0`: with no bus there is nothing to fold, and the
	// sweep — unlike every other subcommand — treats a missing log as success.
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return 0
	}
	s := &sweep{
		paths:  paths,
		cwd:    cwd,
		stderr: stderr,
		o:      o.withDefaults(),
		pid:    strconv.Itoa(os.Getpid()),
	}
	s.store = storePath(s.o.StorePath, s.o.Getenv)
	s.lockd = filepath.Join(filepath.Dir(s.store), "ratings.lock.d")
	if err != nil {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: rate: %s: %v\n", paths.Log, msg)
		return code
	}

	raws := make([]any, len(events))
	for i, ev := range events {
		raws[i] = ev.Doc
	}
	repo, ghRepo := s.repoScopes()
	s.ghRepo = ghRepo

	records, code := s.foldRecords(raws, repo)
	if code != 0 {
		return code
	}

	dir := filepath.Dir(s.store)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		say(stderr, "crew: rate: %s: %v\n", dir, err)
		return exitFailure
	}
	// `crew rate --sweep-all` also discovers repos from here, so a repo swept
	// once stays found wherever it lives on disk.
	if err := appendRegistry(filepath.Join(dir, "repos"), paths.Common); err != nil {
		say(stderr, "crew: rate: %s: %v\n", filepath.Join(dir, "repos"), err)
		return exitFailure
	}

	// --- lock window 1: read the store to decide which calls to skip --------
	snapshot, code := s.lockedFold()
	if code != 0 {
		return code
	}

	// --- t2: the GitHub reconcile, unlocked ---------------------------------
	// The lock is never held across the network: on a repo with many
	// historical runs the reconcile is minutes long, and every other repo's
	// sweep would block on it.
	patches, ghFailures := s.reconcile(records, s.plan(records, snapshot))

	// --- lock window 2: merge forward against a FRESH read, append the diff --
	if code := s.lockedMerge(records, patches); code != 0 {
		return code
	}

	// stdout stays clean (`--report` renders the store, nothing else) — an
	// expired token or a GitHub outage is signalled on stderr only.
	if ghFailures > 0 {
		say(stderr, "crew: rate: %d gh call(s) failed; those rows kept their stored t2\n", ghFailures)
	}
	return 0
}

// foldRecords runs the arm's run fold over the whole bus. The burn table
// reaches it as a priced map because jq cannot call `_burn_weight`: each
// distinct model+effort of the dispatch rows resolves once, here.
func (s *sweep) foldRecords(raws []any, repo string) (any, int) {
	records, err := jqrun.RunAny(recordsProgram, raws, s.clock(), map[string]any{
		"repo":    repo,
		"costmap": s.costmap(raws),
	})
	if err != nil {
		say(s.stderr, "crew: rate: %s: %v\n", s.paths.Log, err)
		return nil, exitType
	}
	return records, 0
}

// costmap prices every distinct engine+model+effort of the dispatch rows, keyed
// "engine\tmodel\teffort" because opus prices per effort (its class follows
// effort). A model no burnClasses glob matches takes its selection-class floor.
func (s *sweep) costmap(raws []any) map[string]any {
	settings := s.settings()
	if settings == "" {
		say(s.stderr, "%s\n", burnWarn)
		return map[string]any{}
	}
	rules := burnRules(settings)

	seen := map[string]bool{}
	out := map[string]any{}
	for _, ev := range raws {
		if kind := fieldText(ev, "kind"); kind != "dispatch" {
			continue
		}
		model := fieldText(ev, "model")
		if model == "" {
			continue
		}
		engine, effort := fieldText(ev, "engine"), fieldText(ev, "effort")
		key := engine + "\t" + model + "\t" + effort
		if seen[key] {
			continue
		}
		seen[key] = true
		if class, weight, ok := burnWeight(rules, model, effort); ok {
			out[key] = []any{class, weight}
		} else if class, weight, ok := classes.FloorClass([]byte(settings), engine, model); ok {
			out[key] = []any{class, weight}
		}
	}
	return out
}

// settings resolves the burn table once, stderr dropped: the arm resolves it
// before the fold so no per-model lookup forks the resolver again.
func (s *sweep) settings() string {
	bin := s.o.Getenv("DISPATCH_CONFIG_BIN")
	if bin == "" {
		bin = "dispatch-config"
	}
	out, _ := s.o.Run(bin)
	return out
}

// repoScopes is the arm's `_origin_repo` / `_origin_github_repo` pair plus the
// toplevel-basename fallback. Only a github.com origin yields a gh_repo, and
// gh_repo alone gates the reconcile.
func (s *sweep) repoScopes() (repo, ghRepo string) {
	url := s.git("config", "--get", "remote.origin.url")
	repo = git.RepoSlug(url)
	switch {
	case strings.HasPrefix(url, "https://github.com/"), strings.HasPrefix(url, "git@github.com:"):
		ghRepo = repo
	}
	if repo == "" {
		if top := s.git("rev-parse", "--show-toplevel"); top != "" {
			repo = filepath.Base(top)
		}
	}
	return repo, ghRepo
}

// plan is the skip-decision fold: per call, not per run (spec §Per-call
// finality), because a run-level skip would strand any field whose own call
// failed on the sweep that first saw the merge.
func (s *sweep) plan(records, snapshot any) []any {
	out, err := jqrun.RunAny(planProgram, []any{}, s.clock(), map[string]any{
		"records": records,
		"snap":    snapshot,
	})
	if err != nil {
		say(s.stderr, "crew: rate: %s: %v\n", s.store, err)
		return nil
	}
	rows, _ := out.([]any)
	return rows
}

// lockedFold is lock window 1: the store read that decides which calls to
// skip. A store the fold cannot read is the arm's `[]` — `2>/dev/null || true`
// hid that a torn line costs every row to every reader.
func (s *sweep) lockedFold() ([]any, int) {
	if !lock.Acquire(s.lockd, s.pid) {
		say(s.stderr, "%s\n", busy)
		return nil, exitFailure
	}
	defer lock.Release(s.lockd)
	return foldStore(s.store), 0
}

// lockedMerge is lock window 2: a fresh fold, the merge-forward, and the batch
// append — all under the lock, none of it across the network. The release is
// deferred, so a fold or write failure cannot strand the lock either.
func (s *sweep) lockedMerge(records any, patches map[string]any) int {
	if !lock.Acquire(s.lockd, s.pid) {
		say(s.stderr, "%s\n", busy)
		return exitFailure
	}
	defer lock.Release(s.lockd)

	out, err := jqrun.RunAny(mergeProgram, []any{}, s.clock(), map[string]any{
		"records": records,
		"stored":  foldStore(s.store),
		"patches": patches,
	})
	if err != nil {
		say(s.stderr, "crew: rate: %s: %v\n", s.store, err)
		return exitType
	}
	rows, _ := out.([]any)
	if err := appendBatch(s.store, rows); err != nil {
		say(s.stderr, "crew: rate: %s: %v\n", s.store, err)
		return exitFailure
	}
	return 0
}

// reconcile is the unlocked t2 phase: the gh calls, the local revert probe,
// and the per-run patch each of them contributes to the merge.
func (s *sweep) reconcile(records any, rows []any) (map[string]any, int) {
	patches := map[string]any{}
	ghFailures := 0
	for _, row := range rows {
		url := fieldText(row, "pr_url")
		if !prURLInRepo(url, s.ghRepo) {
			continue
		}
		owner, name := splitSlug(s.ghRepo)
		number := url[strings.LastIndex(url, "/")+1:]

		patch := map[string]any{}
		tried, ok := false, true
		mCommit := fieldText(row, "s_commit")
		mAt := fieldText(row, "s_merged")

		if jsonval.Truthy(field(row, "do_view")) {
			tried = true
			// Always with the URL: a bare `gh pr view` resolves the sweeping
			// checkout's own branch and would write that PR into every row.
			gh, callOK := s.gh("pr", "view", url,
				"--json", "state,closedAt,mergedAt,mergeCommit,commits,reviews")
			if callOK {
				next, err := s.patch(viewProgram, gh, map[string]any{"p": patch})
				if err != nil {
					ghFailures++
					continue
				}
				patch = next
				mCommit = fieldText(patch, "merge_commit")
				mAt = fieldText(patch, "merged_at_ms")
			} else {
				ok = false
				ghFailures++
			}
		}

		if jsonval.Truthy(field(row, "do_actions")) {
			tried = true
			gh, callOK := s.gh("api", "--method", "GET", "repos/"+owner+"/"+name+"/actions/runs",
				"-f", "branch="+fieldText(row, "branch"), "-F", "per_page=100")
			if callOK {
				next, err := s.patch(actionsProgram, gh, map[string]any{
					"p":  patch,
					"t0": field(row, "t0_ms"),
					"we": field(row, "win_end"),
				})
				if err != nil {
					ghFailures++
					continue
				}
				patch = next
			} else {
				ok = false
				ghFailures++
			}
		}

		if jsonval.Truthy(field(row, "do_threads")) {
			tried = true
			gh, callOK := s.gh("api", "graphql", "-f", "query="+threadsQuery,
				"-F", "owner="+owner, "-F", "name="+name, "-F", "number="+number)
			if callOK {
				next, err := s.patch(threadsProgram, gh, map[string]any{"p": patch})
				if err != nil {
					ghFailures++
					continue
				}
				patch = next
			} else {
				ok = false
				ghFailures++
			}
		}

		// `reverted` — local, no API call. The existence probe is what keeps
		// "not reverted" distinguishable from "merge commit not in this
		// checkout": without it every unfetched repo would report a clean false.
		if mCommit != "" && mAt != "" {
			if rev, known := s.reverted(mCommit, mAt); known {
				patch["reverted"] = rev
			}
		}
		if tried {
			patch["last_query_ok"] = ok
		}
		patches[fieldText(row, "run_id")] = patch
	}
	return patches, ghFailures
}

// reverted probes this checkout: a merge commit that is not here reads as
// unknown (null), never as a clean false.
func (s *sweep) reverted(commit, mergedAt string) (bool, bool) {
	if !s.gitOK("cat-file", "-e", commit+"^{commit}") {
		return false, false
	}
	// `--since` takes approxidate or @<epoch SECONDS>, and silently falls back
	// rather than erroring on a bare 13-digit millisecond value.
	ms, err := strconv.ParseFloat(mergedAt, 64)
	if err != nil {
		return false, false
	}
	since := "@" + strconv.FormatInt(int64(ms)/1000, 10)
	out := s.git("log", "--all", "--no-show-signature", "--since="+since,
		"--grep=This reverts commit "+commit, "--max-count=1")
	return out != "", true
}

// gh runs one gh call: stdout captured, stderr dropped, and a call counts as
// failed on a non-zero exit OR empty stdout (`_gh_json` prints nothing on
// failure, and every call site tests `-n "$out"`).
func (s *sweep) gh(args ...string) (any, bool) {
	out, err := s.o.Gh.Run(context.Background(), args...)
	if err != nil || out == "" {
		return nil, false
	}
	return parseOne(out)
}

// patch runs one gh-ingest fold. The arm piped gh's value into `jq -c`, so
// each fold opens with `$gh |` (jqrun hands its input as an array). A fold
// that cannot use the response is the arm's dead pipeline: counted as a failed
// call, not a reason to kill the sweep.
func (s *sweep) patch(prog string, gh any, vars map[string]any) (map[string]any, error) {
	in := map[string]any{"gh": gh}
	maps.Copy(in, vars)
	out, err := jqrun.RunAny(prog, []any{}, s.clock(), in)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("fold produced %T, want an object", out)
	}
	return m, nil
}

// foldStore is the arm's `jq -s -c "$store_fold" "$store" 2>/dev/null || true`
// and `:-[]`: a missing, empty, torn or unusable store folds to [].
func foldStore(store string) []any {
	data, err := os.ReadFile(store)
	if err != nil {
		return []any{}
	}
	rows, err := decodeAll(bytes.NewReader(data))
	if err != nil {
		return []any{}
	}
	if rows == nil {
		rows = []any{}
	}
	folded, err := jqrun.RunAny(dedupeProgram, rows, 0, nil)
	if err != nil {
		return []any{}
	}
	out, ok := folded.([]any)
	if !ok {
		return []any{}
	}
	return out
}

// appendBatch is the arm's `dd bs=1048576 iflag=fullblock >>"$store"`: the
// whole batch in one O_APPEND write, so a concurrent `--report` reader never
// sees half a row. The guarantee holds while one write() holds, and an empty
// batch still creates the file the way the shell redirect did.
func appendBatch(store string, rows []any) error {
	var buf bytes.Buffer
	for _, row := range rows {
		if err := encode(&buf, row, false); err != nil {
			return err
		}
	}
	batch := buf.Bytes()
	f, err := os.OpenFile(store, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(batch); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// appendRegistry is `grep -qxF "$common" "$registry" || printf '%s\n' …`: an
// exact whole-line check, then a one-line append. The arm's redirect ran under
// `set -e`, so a registry that cannot be opened aborted the sweep before any
// store write: a repo that never enters the registry is invisible to every
// later `--sweep-all`.
func appendRegistry(registry, line string) error {
	if data, err := os.ReadFile(registry); err == nil {
		for _, have := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if have == line {
				return nil
			}
		}
	}
	// A read error is not fatal: `grep -qxF … 2>/dev/null ||` reads a failure to
	// read as "the line is not there" and appends, which is how a write-only
	// registry still gets updated.
	f, err := os.OpenFile(registry, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return nil
}

// prURLPattern is the arm's `_pr_url_in_repo` regex.
var prURLPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/[0-9]+$`)

// prURLInRepo is `_pr_url_in_repo`: a pr_url is unvalidated worker-written
// text, so it alone must never pick the repo gh is pointed at.
func prURLInRepo(url, ghRepo string) bool {
	if ghRepo == "" {
		return false
	}
	m := prURLPattern.FindStringSubmatch(url)
	return m != nil && m[1]+"/"+m[2] == ghRepo
}

// splitSlug is `${gh_repo%%/*}` / `${gh_repo#*/}`.
func splitSlug(slug string) (owner, name string) {
	i := strings.Index(slug, "/")
	return slug[:i], slug[i+1:]
}

func (s *sweep) git(args ...string) string {
	return git.Output(context.Background(), s.o.Git, "", append([]string{"-C", s.cwd}, args...)...)
}

// gitOK is the one call the arm judges by exit status alone: `cat-file -e`
// succeeds by printing nothing, so stdout says nothing about it.
func (s *sweep) gitOK(args ...string) bool {
	_, err := s.o.Git.Run(context.Background(), "", append([]string{"-C", s.cwd}, args...)...)
	return err == nil
}

// kindOf and fieldText are jq's `(.field // "")` for the bus fields the arm
// reads as text; a non-string reaches the fold as its JSON text, which is what
// the arm's `@tsv` printed.
func fieldText(ev any, key string) string {
	v := field(ev, key)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return jsonval.RawText(v)
}

// clockSeconds is jqrun's frozen `now`, at microsecond precision so a fold's
// `now*1000|floor` lands where jq's millisecond stamps land.
func clockSeconds(o Options) float64 {
	if o.Clock.Now == nil {
		o.Clock.Now = time.Now
	}
	return float64(o.Clock.Now().UnixMicro()) / 1e6
}
