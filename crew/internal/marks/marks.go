// Package marks holds the delivered-marks file of #290: one compact JSON
// object per crew and session, {sender: ts of the newest msg from that sender
// this session has been handed}, so a reply taken through the straggler fold is
// not handed back by the next read.
//
// `crew inbox` is the first Go writer; the bash `await`, `stall-watch --unread`
// and `nudge` readers still read and write the same file through crew.sh's
// `_await_state`/`_await_marks`/`_await_record`. The path, the content and the
// merge are therefore the helpers', op for op: the two jq programs here are
// their programs verbatim, and the file name is the helper's sanitized key plus
// the POSIX cksum of the unsanitized one.
package marks

import (
	"bytes"
	"os"
	"strconv"
	"strings"

	_ "embed"

	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed read.jq
var readProgram string

//go:embed record.jq
var recordProgram string

// dirName is the await directory under the crew dir, and tmpPrefix the mktemp
// template `_await_record` gives its staging file.
const (
	dirName   = "await"
	tmpPrefix = ".st."
)

// Path is `_await_state`: dir/await/<key with every [^A-Za-z0-9._-] byte mapped
// to _>.<cksum of the unsanitized key>, for key "<crew>-<me>". The cksum keeps
// two ids that sanitize alike from colliding, and it is the checksum of the
// *key*, so a crew and an agent that swap bytes get different files.
func Path(dir, crew, me string) string {
	key := crew + "-" + me
	var name strings.Builder
	name.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			name.WriteByte(c)
		default:
			name.WriteByte('_')
		}
	}
	return dir + "/" + dirName + "/" + name.String() + "." + strconv.FormatUint(uint64(identity.CKsum([]byte(key))), 10)
}

// Read is `_await_marks`: the file as one JSON object, with a missing,
// unreadable, torn or non-object file reading as {} — redeliver rather than go
// blind. jq's `map(objects) | add // {}` drops non-objects and lets the last
// object holding a key win (`add` folds with `+`, and jq's `+` on objects is
// shallow), so it runs through jqrun rather than being re-typed in Go.
func Read(dir, crew, me string) jsonv.Value {
	b, err := os.ReadFile(Path(dir, crew, me))
	if err != nil {
		return jsonv.Object()
	}
	vs, err := jsonv.DecodeStream(bytes.NewReader(b))
	if err != nil {
		return jsonv.Object()
	}
	old, err := jqrun.Run(readProgram, vs, 0, nil)
	if err != nil {
		return jsonv.Object()
	}
	return old
}

// Record is `_await_record`: raise each sender's mark to the newest of msgs,
// over the marks already held, and replace the file atomically. Best effort —
// every failure path leaves the old file in place and only means redelivery.
// The reduce is the helper's, so a msg whose `from` is not a string fails the
// whole write rather than skipping that one msg, exactly as `jq` exiting 5
// inside the helper's `if` does.
func Record(dir, crew, me string, msgs []jsonv.Value) bool {
	if err := os.MkdirAll(dir+"/"+dirName, 0o755); err != nil {
		return false
	}
	tmp, err := os.CreateTemp(dir+"/"+dirName, tmpPrefix+"*")
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	out, err := jqrun.Run(recordProgram, msgs, 0, map[string]jsonv.Value{"old": Read(dir, crew, me)})
	if err == nil {
		// jq -sc writes the object and its newline; one Write keeps the file from
		// ever holding a half value.
		_, err = tmp.Write(append(jsonv.Append(nil, out, jsonv.Options{}), '\n'))
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false
	}
	return os.Rename(tmp.Name(), Path(dir, crew, me)) == nil
}
