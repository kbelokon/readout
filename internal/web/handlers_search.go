package web

import (
	"net/http"

	"github.com/kbelokon/readout/internal/web/templates"
)

// search renders the search page: the rich search body -- the tools-form with
// the resource-type checkboxes, the scope chips, the result cards with snippet
// highlights + label chips, the count footer, and the per-cluster error articles
// -- assembled in buildSearchView and rendered by templates.Search.
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
