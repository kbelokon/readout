package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func podColor(name string) string {
	return fmt.Sprintf("log-c%d", crc32.ChecksumIEEE([]byte(name))%8)
}

func humanTitle(value string) string {
	words := strings.Fields(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(value))
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// capitalizeWord upper-cases the first byte and lower-cases the rest, so a
// camelCase section key like "eventTime" renders as the title "Eventtime".
func capitalizeWord(value string) string {
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + strings.ToLower(value[1:])
}

func pluralizeKind(kind string) string {
	if strings.HasSuffix(kind, "s") {
		return kind + "es"
	}
	if strings.HasSuffix(kind, "y") {
		return strings.TrimSuffix(kind, "y") + "ies"
	}
	return kind + "s"
}

func sortIcon(sortValue, column string) string {
	switch sortValue {
	case column:
		return ` <span class="icon">` + icon("chevron-down") + `</span>`
	case column + ":desc":
		return ` <span class="icon">` + icon("chevron-down") + `</span>`
	default:
		return ""
	}
}

func createdSortParam(current string) string {
	if current == "Created" {
		return "Created:desc"
	}
	return "Created"
}

func pluralS(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func namespaceEmptyText(namespace string, allNamespaces bool) string {
	if namespace != "" && !allNamespaces {
		return `in namespace "` + html.EscapeString(namespace) + `" `
	}
	return ""
}

func appLabelClass(key string) string {
	if strings.HasPrefix(key, "app.kubernetes.io/") {
		return " ro-label-app"
	}
	return ""
}

func cellClass(table *kube.Table, idx int, cell any) string {
	if idx < 0 || idx >= len(table.Columns) {
		return ""
	}
	return kube.CellClass(table.Resource.Plural, table.Columns[idx].Name, cell)
}

func cpuFormat(cell any) string {
	f, ok := numericCell(cell)
	if !ok {
		return fmt.Sprint(cell)
	}
	return fmt.Sprintf("%.0fm", f*1000)
}

func memoryMiBFormat(cell any) string {
	f, ok := numericCell(cell)
	if !ok {
		return fmt.Sprint(cell)
	}
	return fmt.Sprintf("%.0f", f/(1024*1024))
}

func numericCell(cell any) (float64, bool) {
	switch v := cell.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		return f, err == nil
	}
}

func readyClass(value string) string {
	left, right, ok := strings.Cut(value, "/")
	if !ok {
		return ""
	}
	if left == "0" {
		return "has-text-danger"
	}
	if left == right {
		return "has-text-success"
	}
	return "has-text-warning"
}

func formatTimestamp(value string) string {
	return strings.TrimSuffix(strings.ReplaceAll(value, "T", " "), "Z")
}

func (s *Server) ageClass(value string) string {
	if value == "" {
		return "age-old"
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "age-old"
	}
	seconds := s.clock().Sub(t).Seconds() - 60
	if seconds < 0 {
		seconds = 0
	}
	fraction := seconds / (24 * time.Hour).Seconds()
	switch {
	case fraction < 0.10:
		return "age-fresh"
	case fraction < 0.35:
		return "age-recent"
	case fraction < 0.65:
		return "age-day"
	case fraction < 1.0:
		return "age-week"
	default:
		return "age-old"
	}
}

func assetHashes(fsys fs.FS) map[string]string {
	result := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		result[name] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return result
}

func theme(r *http.Request, cfg *config.Config) string {
	if cookie, err := r.Cookie("theme"); err == nil && allowedTheme(cookie.Value, cfg) {
		return cookie.Value
	}
	if allowedTheme(cfg.DefaultTheme, cfg) {
		return cfg.DefaultTheme
	}
	options := themeOptions(cfg)
	if len(options) > 0 {
		return options[0]
	}
	return "dark"
}

// apiVersionParam reads the resource-type pin from the request, accepting BOTH
// the camelCase `apiVersion` spelling AND the snake_case `api_version` spelling.
// camelCase wins when both are present, so a request that only sets `apiVersion`
// keeps its behavior; the `api_version` spelling is the kubectl-style fallback.
func apiVersionParam(r *http.Request) string {
	if v := r.URL.Query().Get("apiVersion"); v != "" {
		return v
	}
	return r.URL.Query().Get("api_version")
}

func themeExplicit(r *http.Request) bool {
	if _, err := r.Cookie("theme"); err == nil {
		return true
	}
	return r.URL.Query().Get("theme") != ""
}

func activeClass(active bool) string {
	if active {
		return " is-active"
	}
	return ""
}

func activeAttr(active bool) string {
	if active {
		return ` class="is-active"`
	}
	return ""
}

// truncate shortens value to at most max characters (plus a "..." ellipsis),
// preferring to break on the last space. It counts and slices in CODEPOINT
// (rune) space, so an annotation value with multi-byte runes is never cut
// mid-rune (a byte slice could emit invalid UTF-8). The leeway / word-boundary /
// ellipsis behaviour is unchanged.
func truncate(value string, max int) string {
	const leeway = 5
	runes := []rune(value)
	if len(runes) <= max+leeway {
		return value
	}
	if max <= 3 {
		return string(runes[:max])
	}
	prefix := string(runes[:max-3])
	if idx := strings.LastIndex(prefix, " "); idx >= 0 {
		prefix = prefix[:idx]
	}
	return prefix + "..."
}

func splitOwnerTitle(title string) (string, string) {
	kind, name, ok := strings.Cut(title, "/")
	if !ok {
		return "", ""
	}
	return kind, name
}

func allowedTheme(value string, cfg *config.Config) bool {
	if value == "" {
		return false
	}
	for _, option := range themeOptions(cfg) {
		if value == option {
			return true
		}
	}
	return false
}

func themeOptions(cfg *config.Config) []string {
	if len(cfg.ThemeOptions) > 0 {
		return cfg.ThemeOptions
	}
	return []string{"dark", "light"}
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstSlice[T any](value, fallback []T) []T {
	if len(value) > 0 {
		return value
	}
	return fallback
}

func addQuery(u *url.URL, key, value string) string {
	clone := *u
	q := clone.Query()
	q.Set(key, value)
	clone.RawQuery = queryEncodeKeepParens(q)
	return clone.String()
}

// queryEncodeKeepParens URL-encodes the query values but leaves parentheses
// literal, so selector links like `?selector=app(in)(a,b)` stay readable in the
// address bar instead of showing %28/%29.
func queryEncodeKeepParens(values url.Values) string {
	return strings.NewReplacer("%28", "(", "%29", ")").Replace(values.Encode())
}

func resourceListBaseURL(u *url.URL) *url.URL {
	clone := *u
	path := strings.TrimSuffix(strings.TrimRight(clone.Path, "/"), "/_table")
	clone.Path = path
	return &clone
}

func partialResourceListURL(r *http.Request) string {
	clone := *r.URL
	clone.Path = strings.TrimRight(clone.Path, "/") + "/_table"
	return clone.String()
}

func nameColumn(table *kube.Table) int {
	for i, col := range table.Columns {
		if col.Name == "Name" {
			return i
		}
	}
	return 0
}

func cellString(row kube.Row, idx int) string {
	if idx < 0 || idx >= len(row.Cells) {
		return ""
	}
	return cellDisplayString(row.Cells[idx])
}

func cellDisplayString(cell any) string {
	if cell == nil {
		return ""
	}
	return fmt.Sprint(cell)
}

func resourceHref(cluster string, rt *kube.ResourceType, namespace, name string) string {
	if rt.Namespaced {
		return fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(rt.Plural), url.PathEscape(name))
	}
	return fmt.Sprintf("/clusters/%s/%s/%s", url.PathEscape(cluster), url.PathEscape(rt.Plural), url.PathEscape(name))
}

func objectDownloadYAMLHref(cluster, namespace string, object *kube.Object) string {
	if namespace != "" {
		return fmt.Sprintf("/clusters/%s/namespaces/%s/%s/%s?download=yaml", url.PathEscape(cluster), url.PathEscape(namespace), url.PathEscape(object.Resource.Endpoint()), url.PathEscape(object.Name()))
	}
	return fmt.Sprintf("/clusters/%s/%s/%s?download=yaml", url.PathEscape(cluster), url.PathEscape(object.Resource.Endpoint()), url.PathEscape(object.Name()))
}

// nestedString reads a string at the given path from a generic browsed-resource
// object map via the apimachinery accessor (empty when absent or non-string). A
// thin wrapper so the row-object reads stay one-liners; the typed accessor does
// the navigation.
func nestedString(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

func maskSecret(obj map[string]any) {
	if data, ok := obj["data"].(map[string]any); ok {
		for key := range data {
			data[key] = kube.SecretContentHidden
		}
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["annotations"] = map[string]any{"annotations-hidden": "by-readout"}
}

// containerNames returns a pod's container names (regular then init), for the
// logs tab's container picker. Pod is a fixed kind, so the object is decoded
// once into a corev1.Pod and the names are read off the typed PodSpec.
func containerNames(obj map[string]any) []string {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &pod); err != nil {
		return nil
	}
	var names []string
	for i := range pod.Spec.Containers {
		if name := pod.Spec.Containers[i].Name; name != "" {
			names = append(names, name)
		}
	}
	for i := range pod.Spec.InitContainers {
		if name := pod.Spec.InitContainers[i].Name; name != "" {
			names = append(names, name)
		}
	}
	return names
}
