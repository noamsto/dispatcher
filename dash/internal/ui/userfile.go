package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// jsonObject is a JSON object that keeps key order; values are nested
// *jsonObject or json.RawMessage, so number and string text round-trips.
type jsonObject struct {
	keys []string
	vals map[string]any
}

func newJSONObject() *jsonObject {
	return &jsonObject{vals: map[string]any{}}
}

func (o *jsonObject) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsonObject) remove(k string) {
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

func parseObject(raw []byte) (*jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object, got %v", tok)
	}
	obj := newJSONObject()
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := keyTok.(string) // the decoder only yields strings in key position
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if v[0] == '{' {
			sub, err := parseObject(v)
			if err != nil {
				return nil, err
			}
			obj.set(key, sub)
			continue
		}
		obj.set(key, v)
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, err
	}
	return obj, nil
}

func (o *jsonObject) encode(buf *bytes.Buffer) error {
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		switch v := o.vals[k].(type) {
		case *jsonObject:
			if err := v.encode(buf); err != nil {
				return err
			}
		case json.RawMessage:
			buf.Write(v)
		}
	}
	buf.WriteByte('}')
	return nil
}

func parseDoc(doc []byte) (*jsonObject, error) {
	if len(bytes.TrimSpace(doc)) == 0 {
		return newJSONObject(), nil
	}
	return parseObject(doc)
}

func renderDoc(root *jsonObject) ([]byte, error) {
	var compact, out bytes.Buffer
	if err := root.encode(&compact); err != nil {
		return nil, err
	}
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// resolveUserPath follows a symlinked user file (home-manager links it into
// dotfiles), including a dangling link, so writes land on the target and
// never replace the link itself.
func resolveUserPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	target, err := os.Readlink(path)
	if err != nil {
		return path
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return target
}

// readUserFile returns the file's bytes; a missing file is (nil, false, nil).
func readUserFile(path string) (b []byte, existed bool, err error) {
	b, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// setJSONPath returns doc with the value at path set to val, keeping every
// other key and the key order; missing intermediate objects are created.
func setJSONPath(doc []byte, path []string, val json.RawMessage) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("empty settings path")
	}
	root, err := parseDoc(doc)
	if err != nil {
		return nil, err
	}
	cur := root
	for i, k := range path[:len(path)-1] {
		switch next := cur.vals[k].(type) {
		case nil:
			sub := newJSONObject()
			cur.set(k, sub)
			cur = sub
		case *jsonObject:
			cur = next
		default:
			return nil, fmt.Errorf("%v is not an object", path[:i+1])
		}
	}
	cur.set(path[len(path)-1], val)
	return renderDoc(root)
}

// deleteJSONPath removes path from doc, then every parent object the removal
// left empty (never the root). removed is false when the path was absent.
func deleteJSONPath(doc []byte, path []string) (out []byte, removed bool, err error) {
	if len(path) == 0 {
		return nil, false, errors.New("empty settings path")
	}
	root, err := parseDoc(doc)
	if err != nil {
		return nil, false, err
	}
	removed = deleteIn(root, path)
	out, err = renderDoc(root)
	return out, removed, err
}

func deleteIn(o *jsonObject, path []string) bool {
	k := path[0]
	v, ok := o.vals[k]
	if !ok {
		return false
	}
	if len(path) == 1 {
		o.remove(k)
		return true
	}
	sub, ok := v.(*jsonObject)
	if !ok || !deleteIn(sub, path[1:]) {
		return false
	}
	if len(sub.keys) == 0 {
		o.remove(k)
	}
	return true
}

// writeFileAtomic writes b to path via a temp file in the same directory and
// a rename, keeping an existing file's permission bits (new files get 0644).
func writeFileAtomic(path string, b []byte) (err error) {
	mode := fs.FileMode(0o644)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// restoreUserFile puts back what readUserFile returned.
func restoreUserFile(path string, prev []byte, existed bool) error {
	if existed {
		return writeFileAtomic(path, prev)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
