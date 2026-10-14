package gitguard

import (
	"errors"
	"io"
)

// GuardUsage is git-guard's one refusal for any argv its grammar rejects.
const GuardUsage = "crew-go: git-guard: usage: crew-go git-guard check|check-cwd|init|commondir|pairs|union|show-url|show-key|show-pair|canon …"

// GuardRequest is one `crew-go git-guard <op> …` call. Dir is the common dir
// the check, init and record ops take; each op reads only its own fields.
type GuardRequest struct {
	Op, Dir, Gitdir, URL, Mode, Key, Value, Root, Rec string
	Aliases                                           []string
}

// RunGuard is `crew-go git-guard <op> …`. A refusal's reason is already on
// stderr; any other error (an unreadable baseline, a failed write) is printed
// here. Either exits 1.
func RunGuard(req GuardRequest, cwd string, stdout, stderr io.Writer) int {
	var err error
	switch req.Op {
	case "check":
		err = Check(req.Dir, req.Gitdir, stderr)
	case "check-cwd":
		err = CheckCwd(req.Dir, cwd, stderr)
	case "init":
		err = Init(req.Dir, stderr)
	case "commondir":
		err = PinCwd(req.Dir, cwd, stderr)
	case "pairs":
		err = printRecords(stdout, func() ([]string, error) { return Pairs(req.Dir, stderr) })
	case "union":
		err = printRecords(stdout, func() ([]string, error) { return Union(req.Dir, stderr) })
	case "show-url":
		_, err = io.WriteString(stdout, ShowURL(req.URL, req.Mode == "proxy"))
	case "show-key":
		_, err = io.WriteString(stdout, ShowKey(req.Key))
	case "show-pair":
		_, err = io.WriteString(stdout, ShowPair(req.Key, req.Value, req.Aliases))
	case "canon":
		_, err = io.WriteString(stdout, Canon(req.Root, req.Rec))
	}
	if err != nil {
		if !errors.Is(err, ErrRefused) {
			say(stderr, "refusing git: %v\n", err)
		}
		return 1
	}
	return 0
}

// printRecords writes each record NUL-terminated.
func printRecords(stdout io.Writer, list func() ([]string, error)) error {
	recs, err := list()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if _, err := io.WriteString(stdout, rec+"\x00"); err != nil {
			return err
		}
	}
	return nil
}
