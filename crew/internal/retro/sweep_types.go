package retro

type scope int

const (
	scopeHarness scope = iota
	scopeRepo
)

type occurrence struct {
	Unit string
	TS   int64
}

// trackedIssue's State and Reason are lower-cased at read.
type trackedIssue struct {
	Number                         int
	Title, State, Reason, ClosedAt string
}

type finding struct {
	Key, Title string
	Scope      scope
	Ignored    bool
	Occ        []occurrence
	Notes      []flatNote
	Issue      *trackedIssue
	Excerpts   []string
	Hits30     int
	Runs30     int
	LastSeen   int64
	Citations  []string
}

type closeEval struct {
	Verdict, Reason, Basis, ClosedAt string
	Fix                              int64
	HitsB, RunsB, HitsA, RunsA       int
}
