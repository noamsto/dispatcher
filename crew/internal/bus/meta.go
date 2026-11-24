package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Meta is a layout-2 crew's meta.json, the pin that makes it layout 2.
type Meta struct {
	Layout            int     `json:"layout"`
	ID                string  `json:"id"`
	Created           int64   `json:"created"`   // ms
	HomeHost          string  `json:"home_host"` // short hostname; the origin host when visiting
	HomeRepo          *string `json:"home_repo"` // RepoTag of the home repo, null when registered outside one
	HomeSlug          string  `json:"home_slug,omitempty"`
	Visiting          bool    `json:"visiting"`
	CreatedBy         string  `json:"created_by"` // "register" | "remote-dispatch"
	DispatcherSession string  `json:"dispatcher_session,omitempty"`
}

// ErrMetaExists means another creator's meta.json is already in place.
var ErrMetaExists = errors.New("meta.json already exists")

// CreateMeta writes m as crews/<m.ID>/meta.json, Layout set to 2. It publishes
// with a hard link so that of concurrent creators exactly one wins and the rest
// get ErrMetaExists; an existing meta.json is never rewritten.
func CreateMeta(state string, m Meta) error {
	if !ValidCrewID(m.ID) {
		return &InvalidCrewIDError{ID: m.ID}
	}
	m.Layout = 2
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dir := StoreDir(state, m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	err = linkNew(dir, ".meta.json.", metaPath(dir), append(data, '\n'))
	if errors.Is(err, fs.ErrExist) {
		return ErrMetaExists
	}
	return err
}

// linkNew writes data to a temp file in dir and hard-links it to path, which
// fails with fs.ErrExist when path is already there. The temp file is always
// removed.
func linkNew(dir, prefix, path string, data []byte) error {
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Link(tmp, path)
}

// ReadMeta reads crews/<id>/meta.json, rejecting one whose id is not its dir's
// or whose layout is not 2.
func ReadMeta(state, id string) (Meta, error) {
	if !ValidCrewID(id) {
		return Meta{}, &InvalidCrewIDError{ID: id}
	}
	return readMeta(metaPath(StoreDir(state, id)), id)
}

func readMeta(path, id string) (Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case m.ID != id:
		return Meta{}, fmt.Errorf("%s: id %q is not its dir's %q", path, m.ID, id)
	case m.Layout != 2:
		return Meta{}, fmt.Errorf("%s: layout %d, want 2", path, m.Layout)
	}
	return m, nil
}

// MetaError is a crew dir whose meta.json is there but fails ReadMeta.
type MetaError struct {
	ID  string
	Err error
}

// ScanMetas is every crews/*/meta.json that ReadMeta accepts, byte-sorted by id,
// and a MetaError for each crew whose meta.json is present but unreadable.
func ScanMetas(state string) ([]Meta, []MetaError) {
	entries, err := os.ReadDir(state + "/crews")
	if err != nil {
		return nil, nil
	}
	var metas []Meta
	var bad []MetaError
	for _, e := range entries {
		if !e.IsDir() || !ValidCrewID(e.Name()) {
			continue
		}
		m, err := ReadMeta(state, e.Name())
		switch {
		case err == nil:
			metas = append(metas, m)
		case !errors.Is(err, fs.ErrNotExist):
			bad = append(bad, MetaError{ID: e.Name(), Err: err})
		}
	}
	return metas, bad
}

// LiveMetas is every crews/*/meta.json that ReadMeta accepts, byte-sorted by id.
func LiveMetas(state string) []Meta {
	metas, _ := ScanMetas(state)
	return metas
}

func liveIDs(state string) []string {
	var ids []string
	for _, m := range LiveMetas(state) {
		ids = append(ids, m.ID)
	}
	return ids
}
