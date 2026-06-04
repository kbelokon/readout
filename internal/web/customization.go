package web

import (
	"os"
	"path/filepath"
)

func loadPartials(root string) map[string]string {
	partials := map[string]string{}
	if root == "" {
		return partials
	}
	for _, name := range []string{"partials/extrahead.html", "partials/footer.html", "extrahead.html", "footer.html"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			partials[name] = string(data)
		}
	}
	if partials["partials/extrahead.html"] == "" {
		partials["partials/extrahead.html"] = partials["extrahead.html"]
	}
	if partials["partials/footer.html"] == "" {
		partials["partials/footer.html"] = partials["footer.html"]
	}
	return partials
}
