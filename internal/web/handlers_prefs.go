package web

import (
	"net/http"
	"strings"

	"github.com/kbelokon/readout/internal/web/templates"
)

func (s *Server) preferences(w http.ResponseWriter, r *http.Request) {
	current := theme(r, &s.cfg)
	data := templates.PreferencesData{Next: r.URL.Query().Get("next")}
	for _, option := range themeOptions(&s.cfg) {
		data.Options = append(data.Options, templates.ThemeOption{Name: option, Selected: option == current})
	}
	s.pageComponent(w, r, "Preferences", templates.Preferences(data))
}

func (s *Server) savePreferences(w http.ResponseWriter, r *http.Request) {
	next := "/preferences"
	if err := r.ParseForm(); err == nil {
		selectedTheme := r.Form.Get("theme")
		if allowedTheme(selectedTheme, &s.cfg) {
			http.SetCookie(w, &http.Cookie{Name: "theme", Value: selectedTheme, Path: "/", SameSite: http.SameSiteLaxMode})
		}
		if raw := r.Form.Get("next"); strings.HasPrefix(raw, "/") {
			next = raw
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
