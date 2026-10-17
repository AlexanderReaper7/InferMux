package swaputil

import (
	"context"
	"errors"
	"net/http"
)

// InferMux: the warden in front of llama-swap cancels a request's context
// for a yield, a safety stop or an unload by hand (0024, 4), and llama-swap's
// access log remembers that context as the client's, since the warden derived
// it before llama-swap saw the request. MarkClientClosed would record such a
// cancel as a client that hung up, 499, while the warden then answered 503.
// The warden's cancel cause is an HTTPError, so it is written here instead,
// and the access log, the activity and InferMux's stats record the status the
// client gets. A file of its own, so a merge from upstream meets only the one
// call in MarkClientClosed.

// answerCut writes the cause of the client context's cancel when it is an
// HTTPError, and reports whether it did. A client that really hung up ends
// its context with context.Canceled, which is none.
func answerCut(w http.ResponseWriter, r *http.Request) bool {
	var answer HTTPError
	if !errors.As(context.Cause(ClientContext(r.Context())), &answer) {
		return false
	}
	SendError(w, r, answer)
	return true
}
