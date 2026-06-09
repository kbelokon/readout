package web

import (
	"net/http"

	"github.com/kbelokon/readout/internal/web/templates"
)

// search renders the redesign search page: the search hero + scope-opts line, the
// per-cluster scope chips (.ro-scope-chip.ok|err), the partial-failure banner, and
// the results table (.ro-table) -- assembled in buildSearchView and rendered by
// templates.Search.
//
// The /search route is param-less, so the shell scope is derived from the QUERY
// via pageComponentWithScope. Multi-namespace searches intentionally render the
// shell at cluster scope: the search body can round-trip "a,b", but sidebar and
// palette links must not point at a fake namespace named "a,b".
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	view, err := s.buildSearchView(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	s.pageComponentWithScope(w, r, "Search", view.Cluster, view.ShellNamespace, templates.Search(toSearchData(&view)))
}
