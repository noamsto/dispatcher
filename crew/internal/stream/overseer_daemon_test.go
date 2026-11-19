package stream

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverseerDaemonWritesTheLogNotStdout(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "alpha ack"))
	r := newOvRig(t)
	o := r.options(okRepos(alpha))
	o.Binary = func() (string, string) { return "/store/crew", "7:9" }
	l := startFleet(t, o, FleetRequest{Overseer: true, Daemon: true})

	lane := filepath.Join(l.root, ovMe)
	var text string
	waitFor(t, "the batch in the log", func() bool {
		chunk, _, _, _, err := readLog(filepath.Join(lane, logDirName), 0)
		text = string(chunk)
		return err == nil && strings.Contains(text, "alpha ack")
	})
	if got := l.out.str(); got != "" {
		t.Errorf("stdout = %q, want nothing", got)
	}
	info, ok := readDaemonInfo(filepath.Join(lane, daemonInfoName))
	if !ok || info.PID != int64(os.Getpid()) || info.StartOffset != 0 || info.Exe != "/store/crew" || info.ExeID != "7:9" {
		t.Errorf("daemon.json = %+v, %v", info, ok)
	}
	l.stop()
	if _, err := os.Stat(filepath.Join(lane, "stream.lock.d")); err == nil {
		t.Error("the lock was not released")
	}
}

func TestOverseerDaemonRefusesALiveHolder(t *testing.T) {
	r := newOvRig(t)
	root := t.TempDir()
	lockd := mkdir(t, filepath.Join(root, ovMe, "stream.lock.d"))
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	writeFile(t, filepath.Join(lockd, "pid"), fmt.Sprintf("%d\n", holder.Process.Pid))

	var out, errb lockedWriter
	o := r.options(okRepos())
	o.LaneRoot = root
	rc := RunFleet(FleetRequest{Overseer: true, Daemon: true}, &out, &errb, o)
	if rc != 1 || !strings.Contains(errb.str(), "another stream is already running") {
		t.Fatalf("rc %d stderr %q", rc, errb.str())
	}
}

func TestOverseerDaemonRefusals(t *testing.T) {
	cases := map[string]FleetRequest{
		"daemon park":     {Overseer: true, Daemon: true, Request: Request{Park: "5"}},
		"daemon follow":   {Overseer: true, Daemon: true, Follow: true},
		"daemon status":   {Overseer: true, Daemon: true, Request: Request{Status: true}},
		"follow status":   {Overseer: true, Follow: true, Request: Request{Status: true}},
		"daemon overseer": {Daemon: true},
		"follow overseer": {Follow: true},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			r := newOvRig(t)
			o := r.options(okRepos())
			o.LaneRoot = t.TempDir()
			var out, errb lockedWriter
			if rc := RunFleet(req, &out, &errb, o); rc != 64 || out.str() != "" || errb.str() == "" {
				t.Errorf("rc %d stdout %q stderr %q", rc, out.str(), errb.str())
			}
		})
	}
}

func TestOverseerDaemonStatusMode(t *testing.T) {
	r := newOvRig(t)
	root := t.TempDir()
	lane := filepath.Join(root, ovMe)
	me := fmt.Sprint(os.Getpid())
	writeFile(t, filepath.Join(mkdir(t, filepath.Join(lane, "stream.lock.d")), "pid"), me+"\n")
	statusOut := func() string {
		o := r.options(okRepos())
		o.LaneRoot = root
		var out, errb lockedWriter
		RunFleet(FleetRequest{Overseer: true, Request: Request{Status: true}}, &out, &errb, o)
		return out.str()
	}
	if got := statusOut(); strings.Contains(got, `"mode"`) {
		t.Errorf("no daemon.json: %q", got)
	}
	writeFile(t, filepath.Join(lane, daemonInfoName), fmt.Sprintf(`{"pid":%s,"started_ms":1,"start_offset":0}`, me))
	if got := statusOut(); !strings.Contains(got, `"mode":"daemon"`) {
		t.Errorf("daemon.json holder pid: %q", got)
	}
	writeFile(t, filepath.Join(lane, daemonInfoName), `{"pid":1,"started_ms":1,"start_offset":0}`)
	if got := statusOut(); strings.Contains(got, `"mode"`) {
		t.Errorf("foreign pid: %q", got)
	}
}
