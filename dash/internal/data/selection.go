package data

// Selection is a tagged union of the entity a view's cursor is focused on.
// A later actions layer (writes, not built here) will consult a view's
// Selection() to decide whether a keypress it did not consume applies.
type Selection interface {
	isSelection()
}

// SettingRow already implements Selection (see types.go) — Path/Origin/
// Editable is exactly the shape an edit action needs.

type Worker struct {
	Crew    string
	Branch  string
	Session string
}

func (Worker) isSelection() {}

type Run struct {
	Crew   string
	Branch string
	T0     int64
}

func (Run) isSelection() {}
