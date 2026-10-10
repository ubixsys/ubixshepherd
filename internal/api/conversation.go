package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/ubixsys/ubixshepherd/internal/convo"
	"github.com/ubixsys/ubixshepherd/internal/redact"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// PathLaneConversation is GET /v1/lanes/{id}/conversation: the lane's whole dialogue, read
// from its agents' own session stores (convo.Thread). Read-only, and the person's alone:
// a transcript holds full tool input and output, so the route is for the operator token
// and browser sessions, not for the front desk or an agent.
//
//	after=SEQ   only items after it (run boundaries always come); pass the answer's cursor
//	limit=N     most items in one answer; More says to ask again
//	expand=SEQ  that one item with its full text, which the other reads clip
//
// A run whose agent keeps no transcript Shepherd can read has its boundary with a note,
// and a closed lane is read from its runs like an open one.
func PathLaneConversation(id int64) string {
	return fmt.Sprintf("%s/%d/conversation", PathLanes, id)
}

// LaneConversation serves PathLaneConversation from the store. It reads session files
// through src and never writes to them; the daemon registers it on
// "GET /v1/lanes/{id}/conversation".
func LaneConversation(st store.Store, src convo.Source) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			conversationError(w, http.StatusBadRequest, errors.New("bad lane id"))
			return
		}
		if _, err := st.Lane(r.Context(), id); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				code = http.StatusNotFound
			}
			conversationError(w, code, err)
			return
		}
		runs, err := st.Runs(r.Context(), id, "", 1000)
		if err != nil {
			conversationError(w, http.StatusInternalServerError, err)
			return
		}
		q := r.URL.Query()
		num := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return max(n, 0) }
		th := convo.Read(r.Context(), src, runs, convo.Options{
			After: num("after"), Limit: num("limit"), Expand: num("expand"),
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(th)
	}
}

func conversationError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(Error{Error: redact.String(err.Error())})
}
