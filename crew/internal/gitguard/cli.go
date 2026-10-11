package gitguard

import (
	"io"
)

const guardUsage = "crew-go: git-guard: usage: crew-go git-guard check|check-cwd|init|commondir|pairs|union|show-url|show-key|show-pair|canon …\n"

// RunGuard is `crew-go git-guard <op> …`, the worktree-git.sh wrappers' entry
// and the bats inspection shims'. A refusal's reason is already on stderr, so
// any error is a bare exit 1.
func RunGuard(args []string, cwd string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageExit(stderr)
	}
	op, args := args[0], args[1:]
	var err error
	switch {
	case op == "check" && (len(args) == 1 || len(args) == 2):
		gitdir := ""
		if len(args) == 2 {
			gitdir = args[1]
		}
		err = Check(args[0], gitdir, stderr)
	case op == "check-cwd" && len(args) == 1:
		err = CheckCwd(args[0], cwd, stderr)
	case op == "init" && len(args) == 1:
		err = Init(args[0], stderr)
	case op == "commondir" && len(args) == 1:
		err = PinCwd(args[0], cwd, stderr)
	case op == "pairs" && len(args) == 1:
		err = printRecords(stdout, func() ([]string, error) { return Pairs(args[0], stderr) })
	case op == "union" && len(args) == 1:
		err = printRecords(stdout, func() ([]string, error) { return Union(args[0], stderr) })
	case op == "show-url" && (len(args) == 1 || len(args) == 2):
		_, err = io.WriteString(stdout, ShowURL(args[0], len(args) == 2 && args[1] == "proxy"))
	case op == "show-key" && len(args) == 1:
		_, err = io.WriteString(stdout, ShowKey(args[0]))
	case op == "show-pair" && len(args) >= 2:
		_, err = io.WriteString(stdout, ShowPair(args[0], args[1], args[2:]))
	case op == "canon" && len(args) == 2:
		_, err = io.WriteString(stdout, Canon(args[0], args[1]))
	default:
		return usageExit(stderr)
	}
	if err != nil {
		return 1
	}
	return 0
}

func usageExit(stderr io.Writer) int {
	_, _ = io.WriteString(stderr, guardUsage)
	return 64
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
