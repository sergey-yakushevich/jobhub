package httpapi

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// Social network icons, compiled into the binary so a deploy stays one file.
// Named after the network value the boards store: icons/<net>.svg.
//
//go:embed icons/*.svg
var iconFS embed.FS

// handleIcon serves one network icon. Public on purpose: the files are brand
// marks, carry no data, and live behind long-lived caching.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("name"), ".svg")
	if strings.ContainsAny(name, "/\\.") {
		http.NotFound(w, r)
		return
	}
	b, err := iconFS.ReadFile("icons/" + name + ".svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.Write(b)
}

// hasIcon mirrors the embedded set; networks outside it fall back to emoji.
func hasIcon(net string) bool {
	_, err := iconFS.ReadFile("icons/" + net + ".svg")
	return err == nil
}

// netIcon is the <img> for a network, or its emoji when no icon is shipped.
// The value is safe by construction: net is checked against the embedded
// files, so nothing user-controlled reaches the attribute.
func (s *Server) netIcon(net string) template.HTML {
	net = strings.ToLower(strings.TrimSpace(net))
	if net == "twitter" {
		net = "x"
	}
	if !hasIcon(net) {
		return template.HTML(template.HTMLEscapeString(networkEmoji(net)))
	}
	return template.HTML(fmt.Sprintf(
		`<img class="nico" src="%s/icons/%s.svg" alt="%s" loading="lazy">`,
		s.BasePath, net, net))
}

// netTag is icon + name, the label the jobs pages print: "<img …> reddit".
func (s *Server) netTag(net string) template.HTML {
	return s.netIcon(net) + template.HTML(" "+template.HTMLEscapeString(strings.ToLower(net)))
}
