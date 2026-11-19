package view

func ClearsText(k Kind) string { return clearsText(k, clearsCtx{CrewID: "c1"}) }

func DispatcherSession(state string, paneState *string, unfinished int) string {
	return dispatcherSession(state, paneState, unfinished)
}
