package plugins

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

var errQuery = errors.New("invalid agent query")

func agentsQuery(r *http.Request) (fleet.AgentQuery, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fleet.AgentQuery{}, errQuery
	}
	for name, values := range q {
		if (name != "after" && name != "limit" && name != "selector" && name != "connected") || len(values) != 1 || values[0] == "" {
			return fleet.AgentQuery{}, errQuery
		}
	}
	out := fleet.AgentQuery{After: q.Get("after"), Limit: 100}
	if out.After != "" && !fleet.ValidID(out.After) {
		return fleet.AgentQuery{}, errQuery
	}
	if q.Has("limit") {
		out.Limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || out.Limit < 1 || out.Limit > 100 {
			return fleet.AgentQuery{}, errQuery
		}
	}
	if q.Has("selector") {
		if out.Selector, err = fleet.ParseSelector(q.Get("selector")); err != nil {
			return fleet.AgentQuery{}, errQuery
		}
	}
	if q.Has("connected") {
		connected, err := strconv.ParseBool(q.Get("connected"))
		if err != nil || (q.Get("connected") != "true" && q.Get("connected") != "false") {
			return fleet.AgentQuery{}, errQuery
		}
		out.Connected = &connected
	}
	return out, nil
}
