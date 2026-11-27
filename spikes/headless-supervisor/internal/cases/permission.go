package cases

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/pi"
)

const (
	denyNoFiles   = "policy: no file creation in this case"
	webDenyReason = "web fetch denied by supervisor policy for example.com"
)

func init() {
	Register(Case{Name: "bash-allow", Run: bashAllow})
	Register(Case{Name: "bash-deny", Run: bashDeny})
	Register(Case{Name: "bash-session", Run: bashSession})
	Register(Case{Name: "outside-write", Run: outsideWrite})
	Register(Case{Name: "outside-read", Run: outsideRead})
	Register(Case{Name: "mcp", Run: mcpCase})
	Register(Case{Name: "web", Run: webCase})
	Register(Case{Name: "ask", Run: askCase})
	Register(Case{Name: "plan", Run: planCase})
	Register(Case{Name: "bash-deny-hook", Engines: []string{"claude"}, Run: bashDenyHook})
	Register(Case{Name: "bash-deny-mcptool", Engines: []string{"claude"}, Run: bashDenyMCPTool})
}

func bashAllow(ctx context.Context, env *CaseEnv) (string, error) {
	rec := newRecorder(byKind(map[string]event.Decision{"bash": decision("allow_once", "")}))
	evs, err := oneShot(ctx, env, event.Options{}, rec.policy, runInstruction(toolsFor(env.Engine), "touch allowed.txt"))
	if err != nil {
		return "", err
	}
	req, ok := rec.find("bash", "touch")
	if !ok {
		return "", fmt.Errorf("no bash Permission arrived (tool ran permission-less or not at all)")
	}
	if err := toolsOK(evs); err != nil {
		return "", err
	}
	if !exists(filepath.Join(env.Work, "allowed.txt")) {
		return "", fmt.Errorf("allowed.txt does not exist after allow_once")
	}
	return fmt.Sprintf("permission bash '%s' → allow_once → tool ok, file exists", req.Detail.Command), nil
}

func bashDeny(ctx context.Context, env *CaseEnv) (string, error) {
	rec := newRecorder(byKind(map[string]event.Decision{"bash": decision("deny", denyNoFiles)}))
	evs, err := oneShot(ctx, env, event.Options{}, rec.policy, runInstruction(toolsFor(env.Engine), "touch denied.txt"))
	if err != nil {
		return "", err
	}
	req, ok := rec.find("bash", "touch")
	if !ok {
		return "", fmt.Errorf("no bash Permission arrived (tool ran permission-less or not at all)")
	}
	if err := toolDenied(evs, denyNoFiles); err != nil {
		return "", err
	}
	if exists(filepath.Join(env.Work, "denied.txt")) {
		return "", fmt.Errorf("denied.txt exists despite deny")
	}
	return fmt.Sprintf("permission bash '%s' → deny → tool_result error carries the reason, file absent", req.Detail.Command), nil
}

func bashSession(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	rec := newRecorder(byKind(map[string]event.Decision{"bash": decision("allow_session", "")}))
	run, err := launch(ctx, env, event.Options{}, rec.policy, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	mark := run.Mark()
	if _, err := run.Turn(runInstruction(t, "touch s1.txt"), turnTimeout); err != nil {
		return "", fmt.Errorf("turn 1: %w", err)
	}
	req, ok := rec.find("bash", "touch")
	if !ok {
		return "", fmt.Errorf("turn 1: no bash Permission arrived")
	}
	if err := toolsOK(run.Events()[mark:]); err != nil {
		return "", fmt.Errorf("turn 1: %w", err)
	}
	before := len(rec.requests())

	mark = run.Mark()
	if _, err := run.Turn(runInstruction(t, "touch s2.txt"), turnTimeout); err != nil {
		return "", fmt.Errorf("turn 2: %w", err)
	}
	evs := run.Events()[mark:]
	if n := len(rec.requests()) - before; n != 0 {
		return "", fmt.Errorf("turn 2 produced %d new Permission(s) despite allow_session", n)
	}
	if len(ofKind(evs, event.KindToolStart)) == 0 {
		return "", fmt.Errorf("turn 2: model made no tool call")
	}
	if err := toolsOK(evs); err != nil {
		return "", fmt.Errorf("turn 2: %w", err)
	}
	for _, f := range []string{"s1.txt", "s2.txt"} {
		if !exists(filepath.Join(env.Work, f)) {
			return "", fmt.Errorf("%s does not exist", f)
		}
	}
	return fmt.Sprintf("permission bash '%s' → allow_session → second call ran with no new Permission, both files exist", req.Detail.Command), nil
}

func outsideWrite(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	target := filepath.Join(env.Outside, "w.txt")
	rec := newRecorder(byKind(map[string]event.Decision{"write": decision("allow_once", "")}))
	prompt := fmt.Sprintf("Use the %s tool to create the file %s with the content: outside-write-ok . %s Then reply done.", t.Write, target, noMoreTools)
	evs, err := oneShot(ctx, env, event.Options{}, rec.policy, prompt)
	if err != nil {
		return "", err
	}
	req, ok := rec.find("write", "w.txt")
	if !ok {
		return "", fmt.Errorf("no write Permission arrived")
	}
	if !strings.HasPrefix(req.Detail.Path, env.Outside) {
		return "", fmt.Errorf("write Permission path %q is not under the outside dir", req.Detail.Path)
	}
	if err := toolsOK(evs); err != nil {
		return "", err
	}
	if !exists(target) {
		return "", fmt.Errorf("outside file does not exist after allow_once")
	}
	return fmt.Sprintf("permission write path under outside dir (%s) → allow_once → tool ok, file exists", filepath.Base(req.Detail.Path)), nil
}

func outsideRead(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	const content = "outside-content-42"
	target := filepath.Join(env.Outside, "outside-content.txt")
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		return "", err
	}
	rec := newRecorder(byKind(map[string]event.Decision{"read": decision("allow_once", "")}))
	prompt := fmt.Sprintf("Use the %s tool to read the file %s and then reply with its exact content. %s", t.Read, target, noMoreTools)
	evs, err := oneShot(ctx, env, event.Options{}, rec.policy, prompt)
	if err != nil {
		return "", err
	}
	req, ok := rec.find("read", "outside-content.txt")
	if !ok {
		return "", fmt.Errorf("no read Permission arrived")
	}
	if !strings.HasPrefix(req.Detail.Path, env.Outside) {
		return "", fmt.Errorf("read Permission path %q is not under the outside dir", req.Detail.Path)
	}
	if err := toolsOK(evs); err != nil {
		return "", err
	}
	for _, e := range ofKind(evs, event.KindToolResult) {
		if strings.Contains(e.Text, content) {
			return "permission read path under outside dir → allow_once → tool result contains " + content, nil
		}
	}
	return "", fmt.Errorf("no tool result contains %q", content)
}

func mcpCase(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	o := event.Options{}
	if env.Engine == "claude" {
		cfg := filepath.Join(env.Work, "mcp.json")
		if err := echoConfig(env, cfg, nil); err != nil {
			return "", err
		}
		o.MCPConfig = cfg
	} else {
		// exposure "direct" declares the tool to the model; the default
		// (codemode) would hide it behind a script tool.
		if err := echoConfig(env, filepath.Join(env.Work, ".pi", "mcp.json"), map[string]any{"exposure": "direct"}); err != nil {
			return "", err
		}
		o.Approve, o.MCP = true, true
	}
	rec := newRecorder(byKind(map[string]event.Decision{"mcp": decision("allow_once", "")}))
	prompt := fmt.Sprintf("Call the %s tool with the argument text set to hello-mcp-17 and reply with what it returned. %s", t.MCP, noMoreTools)
	evs, err := oneShot(ctx, env, o, rec.policy, prompt)
	if err != nil {
		return "", err
	}
	req, ok := rec.find("mcp", "echo")
	if !ok {
		return "", fmt.Errorf("no mcp Permission arrived")
	}
	if req.Detail.Server != "echo" {
		return "", fmt.Errorf("mcp Permission server = %q, want echo", req.Detail.Server)
	}
	if err := toolsOK(evs); err != nil {
		return "", err
	}
	for _, e := range ofKind(evs, event.KindToolResult) {
		if strings.Contains(e.Text, "hello-mcp-17") {
			return "permission mcp server echo → allow_once → tool result echoes hello-mcp-17", nil
		}
	}
	return "", fmt.Errorf("no tool result echoes hello-mcp-17")
}

// webCase: claude exercises allow_session across two WebFetch calls. The pi
// variant is gate-only: pi-web-access reads its config from the same
// directory as pi's own credentials and its fallback chain can reach a
// credentialed provider, so it is not loaded; a stub fetch_content tool stands
// in so the gate has a web tool call to classify, and policy denies it.
func webCase(ctx context.Context, env *CaseEnv) (string, error) {
	if env.Engine == "claude" {
		return webClaude(ctx, env)
	}
	return webPi(ctx, env)
}

func webClaude(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	rec := newRecorder(byKind(map[string]event.Decision{"web": decision("allow_session", "")}))
	// WebFetch is not in the default built-in tool set here; naming it yields a domain:<host> suggestion.
	opts := event.Options{ExtraArgs: []string{"--tools", "Bash,WebFetch"}}
	run, err := launch(ctx, env, opts, rec.policy, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	mark := run.Mark()
	p1 := fmt.Sprintf("Use the %s tool to fetch https://example.com and state its title in one short sentence. %s", t.Web, noMoreTools)
	if _, err := run.Turn(p1, turnTimeout); err != nil {
		return "", fmt.Errorf("turn 1: %w", err)
	}
	req, ok := rec.find("web", "example.com")
	if !ok {
		return "", fmt.Errorf("turn 1: no web Permission for example.com")
	}
	if req.Detail.Domain != "example.com" {
		return "", fmt.Errorf("web Permission domain = %q, want example.com", req.Detail.Domain)
	}
	if err := toolsOK(run.Events()[mark:]); err != nil {
		return "", fmt.Errorf("turn 1: %w", err)
	}
	before := len(rec.requests())

	mark = run.Mark()
	p2 := fmt.Sprintf("Use the %s tool once more to fetch https://example.com/?again=1 and state the first heading. %s", t.Web, noMoreTools)
	if _, err := run.Turn(p2, turnTimeout); err != nil {
		return "", fmt.Errorf("turn 2: %w", err)
	}
	evs := run.Events()[mark:]
	if n := len(rec.requests()) - before; n != 0 {
		return "", fmt.Errorf("turn 2 produced %d new Permission(s) despite allow_session", n)
	}
	if len(ofKind(evs, event.KindToolStart)) == 0 {
		return "", fmt.Errorf("turn 2: model made no tool call")
	}
	if err := toolsOK(evs); err != nil {
		return "", fmt.Errorf("turn 2: %w", err)
	}
	return "permission web domain example.com → allow_session → second fetch of the same domain had no Permission", nil
}

const fetchStub = `import { Type } from "typebox";

// Test double for pi-web-access' fetch_content: never touches the network.
export default function (pi: any) {
	pi.registerTool({
		name: "fetch_content",
		label: "Fetch content",
		description: "Fetch the text content of a web page URL.",
		parameters: Type.Object({ url: Type.String({ description: "The URL to fetch" }) }),
		async execute() {
			return { content: [{ type: "text", text: "stub: network disabled in this spike" }], details: {} };
		},
	});
}
`

func webPi(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	dir := env.State
	if dir == "" {
		dir = env.Work
	}
	stub := filepath.Join(dir, "fetch_content.ts")
	if err := os.WriteFile(stub, []byte(fetchStub), 0o600); err != nil {
		return "", err
	}
	rec := newRecorder(byKind(map[string]event.Decision{"web": decision("deny", webDenyReason)}))
	prompt := fmt.Sprintf("Use the %s tool to fetch https://example.com and state its title. %s", t.Web, noMoreTools)
	evs, err := oneShot(ctx, env, event.Options{Extensions: []string{stub}}, rec.policy, prompt)
	if err != nil {
		return "", err
	}
	req, ok := rec.find("web", "example.com")
	if !ok {
		return "", fmt.Errorf("no web Permission arrived")
	}
	if req.Detail.Domain != "example.com" {
		return "", fmt.Errorf("web Permission domain = %q, want example.com", req.Detail.Domain)
	}
	if err := toolDenied(evs, webDenyReason); err != nil {
		return "", err
	}
	return "partial: permission web domain example.com → deny → denial reached the model; allow not exercised " +
		"because pi-web-access shares pi's credential directory and its gemini fallback could read owner credentials, so a stub fetch_content tool stood in", nil
}

func askCase(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	rec := newRecorder(func(r *event.Request) event.Decision {
		if r.Detail.Kind != "ask" {
			return decision("deny", "unexpected permission kind "+r.Detail.Kind)
		}
		choice := pickOption(r.Detail.Options, "teal")
		key := "answer" // pi: single selected option
		if env.Engine == "claude" {
			key = firstQuestion(r.Detail.Questions)
		}
		return event.Decision{Verdict: "answer", Answers: map[string]string{key: choice}}
	})
	// Three options, the answer last: a lost answer makes the model guess or
	// pick the first, which this case must not mistake for a pass.
	prompt := fmt.Sprintf("Use the %s tool to ask me which colour I prefer, offering exactly three options: Red, Blue and Teal. %s After I answer, reply with the colour I chose.", t.Ask, noMoreTools)
	evs, err := oneShot(ctx, env, event.Options{}, rec.policy, prompt)
	if err != nil {
		return "", err
	}
	req, ok := rec.find("ask", "")
	if !ok {
		return "", fmt.Errorf("no ask Permission arrived")
	}
	askIDs := map[string]bool{}
	for i, e := range evs {
		if e.Kind == event.KindToolStart && e.Tool == t.Ask {
			askIDs[e.ToolUseID] = true
			continue
		}
		isAsk := e.Tool == t.Ask || e.ToolUseID != "" && askIDs[e.ToolUseID]
		if e.Kind != event.KindToolResult || !isAsk {
			continue
		}
		if e.IsError {
			return "", fmt.Errorf("ask tool result is an error: %s", clip(e.Text))
		}
		result := strings.ToLower(e.Text)
		if strings.Contains(result, "teal") {
			return fmt.Sprintf("permission ask options %v → answered Teal → ask tool result carries Teal", req.Detail.Options), nil
		}
		// A non-empty result without Teal is the lost answer itself; the model
		// text cannot redeem it, since it may just echo the offered options.
		if strings.TrimSpace(result) != "" {
			return "", fmt.Errorf("ask tool result lacks Teal: %s", clip(e.Text))
		}
		if after := textOf(evs[i+1:]); tealWord.MatchString(after) && !otherColour.MatchString(after) {
			return fmt.Sprintf("permission ask options %v → answered Teal → ask result empty, assistant text after it names only Teal", req.Detail.Options), nil
		}
		return "", fmt.Errorf("ask tool result empty and the text after it does not name only Teal: %s", clip(textOf(evs[i+1:])))
	}
	return "", fmt.Errorf("no %s tool result arrived: %s", t.Ask, clip(textOf(evs)))
}

var (
	tealWord    = regexp.MustCompile(`(?i)\bteal\b`)
	otherColour = regexp.MustCompile(`(?i)\b(red|blue)\b`)
)

// pickOption returns the option equal to want ignoring case, else want.
func pickOption(opts []string, want string) string {
	for _, o := range opts {
		if strings.EqualFold(o, want) {
			return o
		}
	}
	return want
}

// firstQuestion is the text of the first question in claude's questions array.
func firstQuestion(raw json.RawMessage) string {
	var qs []struct {
		Question string `json:"question"`
	}
	if json.Unmarshal(raw, &qs) == nil && len(qs) > 0 {
		return qs[0].Question
	}
	return "question"
}

func planCase(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	o := event.Options{}
	var prompt string
	if env.Engine == "claude" {
		o.PermissionMode = "plan"
		prompt = fmt.Sprintf("Plan how to create a file named plan.txt containing the word planned, submit the plan with the ExitPlanMode tool and, once it is approved, create the file with the %s tool. %s", t.Write, noMoreTools)
	} else {
		bin, err := pi.ResolveBin()
		if err != nil {
			return "", err
		}
		o.Bin = bin
		o.Extensions = []string{pi.ExampleExtension(bin, "plan-mode")}
		o.ExtraArgs = []string{"--plan"}
		prompt = "Do not change anything yet; reply only with this plan, exactly in this format:\n\nPlan:\n" +
			"1. Create a file named plan.txt containing the word planned using the write tool.\n" +
			"2. Reply with the word done.\n"
	}
	rec := newRecorder(byKind(map[string]event.Decision{
		"plan":  decision("allow_once", ""),
		"write": decision("allow_once", ""),
		"bash":  decision("allow_once", ""),
	}))
	run, err := launch(ctx, env, o, rec.policy, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	run.SkipPending()
	mark := run.Mark()
	if err := run.Send(prompt); err != nil {
		return "", err
	}
	isWork := func(e event.Event) bool {
		return e.Kind == event.KindToolStart && (e.Detail.Kind == "write" || e.Detail.Kind == "bash")
	}
	planSeen := false
	ev, err := run.WaitFor(func(e event.Event) bool {
		if e.Kind == event.KindPermission && e.Detail.Kind == "plan" {
			planSeen = true
		}
		return planSeen && isWork(e)
	}, turnTimeout)
	if err != nil {
		if !planSeen {
			return "", fmt.Errorf("no plan Permission arrived: %w", err)
		}
		return "", fmt.Errorf("plan Permission arrived but no write/bash tool started after it: %w", err)
	}
	// pi executes an approved plan in a followUp turn, so the TurnEnd that
	// counts is the one after the tool started, not the plan turn's.
	if _, err := run.WaitKind(event.KindTurnEnd, turnTimeout); err != nil {
		return "", fmt.Errorf("%s started after the plan but its turn never ended: %w", ev.Tool, err)
	}
	evs := run.Events()[mark:]
	work := map[string]bool{}
	for _, e := range evs {
		if isWork(e) {
			work[e.ToolUseID] = true
		}
	}
	var results []event.Event
	for _, e := range ofKind(evs, event.KindToolResult) {
		if work[e.ToolUseID] {
			results = append(results, e)
		}
	}
	if err := toolsOK(results); err != nil {
		return "", fmt.Errorf("post-approval %s: %w", ev.Tool, err)
	}
	if !exists(filepath.Join(env.Work, "plan.txt")) {
		return "", fmt.Errorf("plan.txt does not exist after the approved plan ran")
	}
	req, _ := rec.find("plan", "")
	return fmt.Sprintf("permission plan options %v → allow_once → %s ran ok after the plan Permission, plan.txt exists", req.Detail.Options, ev.Tool), nil
}

func bashDenyHook(ctx context.Context, env *CaseEnv) (string, error) {
	const reason = "policy: hook denies file creation"
	rec := newRecorder(func(*event.Request) event.Decision { return decision("deny", reason) })
	evs, err := oneShot(ctx, env, event.Options{Hooks: true}, rec.policy, runInstruction(toolsFor(env.Engine), "touch hooked.txt"))
	if err != nil {
		return "", err
	}
	hook := false
	for _, q := range rec.requests() {
		if q.Engine == "claude-hook" && q.Detail.Kind == "bash" && strings.Contains(q.Detail.Command, "touch") {
			hook = true
		}
	}
	if !hook {
		return "", fmt.Errorf("no claude-hook bash Request arrived")
	}
	if err := toolDenied(evs, reason); err != nil {
		return "", err
	}
	if exists(filepath.Join(env.Work, "hooked.txt")) {
		return "", fmt.Errorf("hooked.txt exists despite hook deny")
	}
	return "hook_callback bash 'touch hooked.txt' → deny → tool_result error carries the reason, file absent", nil
}

func bashDenyMCPTool(ctx context.Context, env *CaseEnv) (string, error) {
	cfg := filepath.Join(env.Work, "mcp.json")
	if err := echoConfig(env, cfg, nil); err != nil {
		return "", err
	}
	rec := newRecorder(func(*event.Request) event.Decision { return decision("deny", "supervisor must not be asked") })
	o := event.Options{MCPConfig: cfg, PromptToolMCP: "mcp__echo__permission_prompt"}
	evs, err := oneShot(ctx, env, o, rec.policy, runInstruction(toolsFor(env.Engine), "touch mcpdenied.txt"))
	if err != nil {
		return "", err
	}
	if n := len(rec.requests()); n != 0 {
		return "", fmt.Errorf("%d Permission(s) reached the supervisor; expected the MCP prompt tool to decide", n)
	}
	if err := toolDenied(evs, "denied by mcp permission_prompt"); err != nil {
		return "", err
	}
	if exists(filepath.Join(env.Work, "mcpdenied.txt")) {
		return "", fmt.Errorf("mcpdenied.txt exists despite MCP deny")
	}
	return "permission prompt routed to mcpecho permission_prompt → tool_result error carries its deny message, file absent", nil
}
