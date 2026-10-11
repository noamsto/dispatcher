package reap

import (
	_ "embed"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

//go:embed forget.jq
var forgetProg string

// forget is `_reap_forget`: drop the reaped branch's grants, protocol-dirs and
// leads files and its sessions' await marks, phase-status and
// directive-delivery records, by exact derived path. artifacts/<branch> stays:
// it can hold a --discard patch. Best-effort throughout.
func (r *run) forget(branch string) {
	dir := r.paths.Dir
	parent := ""
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		parent = branch[:i]
	}
	for _, cat := range []string{"grants", "protocol-dirs", "leads"} {
		if chainBad(dir+"/"+cat, parent) != "" {
			continue
		}
		leaf := dir + "/" + cat + "/" + branch
		if fi, err := os.Lstat(leaf); err == nil && fi.Mode().IsRegular() {
			_ = os.Remove(leaf)
		}
	}

	raws, err := readRaws(r.paths.Log)
	if err != nil {
		return
	}
	pairs, err := jqrun.Run(forgetProg, raws, r.nowSec(), map[string]jsonv.Value{"b": jsonv.Str(branch)})
	if err != nil {
		return
	}
	for _, pair := range cells(pairs) {
		c, id := pair[0], pair[1]
		if c == "" || id == "" {
			continue
		}
		_ = os.Remove(marks.Path(dir, c, id))
		_ = os.Remove(marks.LockPath(dir, c, id))
		key := sanitize(id)
		_ = os.Remove(dir + "/phase-status/" + key + "." + strconv.FormatUint(uint64(identity.CKsum([]byte(id))), 10))
		d := dir + "/directive-delivery/" + key
		if fi, err := os.Lstat(d); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if data, err := os.ReadFile(d); err == nil && line2(string(data)) == id {
			_ = os.Remove(d)
		}
	}
}

// sanitize is `tr -c 'A-Za-z0-9._-' '_'`, bytewise.
func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

// line2 is `$(sed -n 2p file)`.
func line2(data string) string {
	lines := strings.SplitN(data, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	return lines[1]
}

// chainBad is `_chain_bad <root> <relpath>`: the first path from root down
// through each component of relpath that is a symlink or exists as a
// non-directory, else "".
func chainBad(root, rel string) string {
	p := root
	parts := []string{""}
	if rel != "" {
		parts = append(parts, strings.Split(rel, "/")...)
	}
	for _, part := range parts {
		if part != "" {
			p += "/" + part
		}
		if fi, err := os.Lstat(p); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
			return p
		}
	}
	return ""
}
