package rosterrender

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// Probes are the side effects the loop cannot compute, injected so the daemon is
// testable without a terminal, an aeye install, or a second build to hop into. A
// nil field is a bug in Default, not a runtime case. The pid probes are not here:
// they come in as a proc.Table.
type Probes struct {
	// Tmux answers the role panes (`list-panes` with rolePaneFormat), the branch
	// stamps the liveness rule reads (exactly as `crew fleet` reads them) and
	// the pane pid probe.
	Tmux tmux.Client
	// AeyeHelp is `timeout 10 aeye --help`: stdout only, stderr dropped.
	AeyeHelp func(ctx context.Context) (string, error)
	// AeyePublish is `timeout 60 aeye publish-diagram <file> --pane <pane>
	// [--open]` with both streams dropped; its status is the arm's.
	AeyePublish func(ctx context.Context, file, pane string, open bool) error
	// ViewerSize is the size in cells of the aeye carousel pane that shows pane's
	// diagrams, or of pane itself while no carousel is open; ok is false when
	// tmux cannot say.
	ViewerSize func(ctx context.Context, pane string) (cols, rows int, ok bool)
	// Resvg rasterizes the finished svg to a PNG of exactly w x h pixels: the
	// diagram is scaled to fit and centred, the margins are transparent. It reads
	// its fonts from $AEYE_D2_FONT_DIR when that is set, like aeye's own resvg.
	Resvg func(ctx context.Context, svg []byte, w, h int) ([]byte, error)
	// InstalledCrew is `_rr_installed_crew` over the given PATH.
	InstalledCrew func(path string) string
	// SpawnDetached starts argv as a detached child — new session, /dev/null
	// stdio, the given environment — without waiting for it.
	SpawnDetached func(argv, env []string) error
	// Exec is the build hop: replace this process. It only returns on failure.
	Exec func(path string, argv, env []string) error
	// Sleep is the poll wait on the wall clock, cut short by a done ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Default wires the real tmux and aeye reads. Each aeye call takes the ctx of
// its caller, so a killed worker never leaves a `timeout 60 aeye` behind it.
func Default() Probes {
	return Probes{
		Tmux: tmux.Exec{},
		AeyeHelp: func(ctx context.Context) (string, error) {
			out, err := exec.CommandContext(ctx, "timeout", "10", "aeye", "--help").Output()
			return string(out), err
		},
		AeyePublish: func(ctx context.Context, file, pane string, open bool) error {
			args := []string{"timeout", "60", "aeye", "publish-diagram", file, "--pane", pane}
			if open {
				args = append(args, "--open")
			}
			return exec.CommandContext(ctx, args[0], args[1:]...).Run()
		},
		ViewerSize: func(ctx context.Context, pane string) (int, int, bool) {
			out, err := tmux.Exec{}.Run(ctx, "list-panes", "-a", "-F", viewerFormat)
			if err != nil {
				return 0, 0, false
			}
			return viewerSize(out, pane)
		},
		Resvg:         resvg,
		InstalledCrew: InstalledCrew,
		SpawnDetached: SpawnDetached,
		Exec: func(path string, argv, env []string) error {
			return syscall.Exec(path, argv, env)
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// viewerFormat is the `list-panes -a` row viewerSize reads.
const viewerFormat = "#{pane_id} #{pane_width} #{pane_height} #{pane_start_command}"

// viewerSize finds pane's viewer in a viewerFormat listing: the aeye pane whose
// start command ends in `<server-pid>-<N>` for pane `%N`, aeye's name for the
// carousel it opens beside that pane. Without one the carousel is not open yet,
// and pane's own size stands in for it, as the carousel splits beside it at
// the same height. A pane the listing does not hold is not ok.
func viewerSize(listing, pane string) (cols, rows int, ok bool) {
	n := strings.TrimPrefix(pane, "%")
	var own [2]int
	found := false
	for line := range strings.Lines(listing) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), " ", 4)
		if len(f) < 3 {
			continue
		}
		w, errW := strconv.Atoi(f[1])
		h, errH := strconv.Atoi(f[2])
		if errW != nil || errH != nil {
			continue
		}
		if f[0] == pane {
			own, found = [2]int{w, h}, true
			continue
		}
		if len(f) == 4 && strings.Contains(f[3], "aeye") {
			last := f[3][strings.LastIndexByte(f[3], ' ')+1:]
			pid, id, cut := strings.Cut(strings.Trim(last, `"'`), "-")
			if cut && id == n && ascii.IsDigits(pid) {
				return w, h, true
			}
		}
	}
	return own[0], own[1], found
}

// resvgTimeout bounds one resvg run so a hung one cannot hold the loop.
const resvgTimeout = 30 * time.Second

// svgRootRe is the root element's opening tag.
var svgRootRe = regexp.MustCompile(`<svg\b[^>]*>`)

var (
	svgWidthRe  = regexp.MustCompile(`\swidth="[^"]*"`)
	svgHeightRe = regexp.MustCompile(`\sheight="[^"]*"`)
)

// sizeSVG sets the root element's width and height to w x h pixels and keeps
// its viewBox, so resvg fits the drawing into that box.
func sizeSVG(svg []byte, w, h int) []byte {
	m := svgRootRe.FindIndex(svg)
	if m == nil {
		return svg
	}
	tag := svgWidthRe.ReplaceAll(svg[m[0]:m[1]], []byte(fmt.Sprintf(` width="%dpx"`, w)))
	tag = svgHeightRe.ReplaceAll(tag, []byte(fmt.Sprintf(` height="%dpx"`, h)))
	return slices.Concat(svg[:m[0]], tag, svg[m[1]:])
}

func resvg(ctx context.Context, svg []byte, w, h int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, resvgTimeout)
	defer cancel()
	var args []string
	if fonts := os.Getenv("AEYE_D2_FONT_DIR"); fonts != "" {
		args = append(args, "--skip-system-fonts", "--use-fonts-dir", fonts)
	}
	cmd := exec.CommandContext(ctx, "resvg", append(args, "-", "-c")...)
	cmd.Stdin = bytes.NewReader(sizeSVG(svg, w, h))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("resvg: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("resvg: %w", err)
	}
	return stdout.Bytes(), nil
}

// rolePanes is the stdout of `tmux list-panes -a -F` with rolePaneFormat. A
// tmux failure is the arm's `|| true`: no rows.
func (o Options) rolePanes(ctx context.Context) string {
	return tmux.Output(ctx, o.Probes.Tmux, "list-panes", "-a", "-F", rolePaneFormat)
}

// SpawnDetached starts argv in its own session with its stdio off /dev/null, so
// the daemon has no controlling terminal and outlives whoever started it — the
// arm's `nohup … </dev/null >/dev/null 2>&1 &`.
func SpawnDetached(argv, env []string) error {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
