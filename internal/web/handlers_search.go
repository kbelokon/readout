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
// The /search route is param-less, so the shell scope is taken from the QUERY
// (?cluster= / ?namespace=) via pageComponentWithScope: with a concrete
// ?cluster= the layout renders that cluster's sidebar + navbar context; with an
// empty or all-clusters scope the layout gates emit no sidebar.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	view, err := s.buildSearchView(r)
	if err != nil {
		s.error(w, r, err)
		return
	}
	s.pageComponentWithScope(w, r, "Search", view.Cluster, view.Namespace, templates.Search(toSearchData(&view)))
}
