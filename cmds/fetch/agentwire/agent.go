// Package agentwire reads agent-ready site announcements from HTTP responses.
package agentwire

import (
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"
)

// AgentLink is a link-value from an HTTP Link field.
type AgentLink struct {
	URL  string   `json:"url"`
	Rel  []string `json:"rel,omitempty"`
	Type string   `json:"type,omitempty"`
}

// Split outside quoted strings and (for Link) URI references.
func splitField(s string, sep byte, links bool) []string {
	var out []string
	start := 0
	var quote, uri, escaped bool
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case escaped:
			escaped = false
		case quote && b == '\\':
			escaped = true
		case !uri && b == '"':
			quote = !quote
		case links && !quote && b == '<':
			uri = true
		case links && !quote && b == '>':
			uri = false
		case !quote && !uri && b == sep:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

func fieldValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.NewReplacer(`\\"`, `"`, `\\`, `\`).Replace(s[1 : len(s)-1])
	}
	return s
}

func ShapedFields(header string) map[string]string {
	out := map[string]string{}
	for _, field := range splitField(header, ',', false) {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			out[strings.TrimSpace(key)] = fieldValue(value)
		}
	}
	return out
}

func AgentLinks(header http.Header) []AgentLink {
	var out []AgentLink
	for _, value := range header.Values("Link") {
		for _, field := range splitField(value, ',', true) {
			target, params, ok := strings.Cut(field, ">")
			if !ok || !strings.HasPrefix(target, "<") || len(target) == 1 {
				continue
			}
			link := AgentLink{URL: target[1:]}
			for _, param := range splitField(params, ';', false) {
				key, val, ok := strings.Cut(param, "=")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "rel":
					link.Rel = strings.Fields(fieldValue(val))
				case "type":
					link.Type = fieldValue(val)
				}
			}
			out = append(out, link)
		}
	}
	return out
}

func (l AgentLink) AnnouncesAgent() bool {
	for _, rel := range l.Rel {
		switch rel {
		case "alternate":
			if l.Type == "text/markdown" || l.Type == "application/json" {
				return true
			}
		case "service-desc", "service-doc", "describedby":
			return true
		}
	}
	return false
}

func MarkdownAgent(body string) (string, map[string]string) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	var fm struct {
		Agent map[string]string `yaml:"agent"`
	}
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		if front, content, ok := strings.Cut(rest, "\n---\n"); ok {
			_ = yaml.Unmarshal([]byte(front), &fm)
			body = content
		}
	}
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## For agents {#agents}" {
			start = i
			continue
		}
		if start >= 0 && (strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ")) {
			return strings.TrimSpace(strings.Join(lines[start:i], "\n")), fm.Agent
		}
	}
	if start >= 0 {
		return strings.TrimSpace(strings.Join(lines[start:], "\n")), fm.Agent
	}
	return fm.Agent["instructions"], fm.Agent
}

// HTMLAgent extracts the visible block and the inert recipe data.
func HTMLAgent(body string) (block, recipe string) {
	root, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", ""
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch attr(n, "id") {
			case "agent-recipe":
				if n.Data == "script" && attr(n, "type") == "text/bashsharp" && n.FirstChild != nil {
					recipe = n.FirstChild.Data
				}
			case "agents":
				var rendered strings.Builder
				_ = html.Render(&rendered, n)
				if n.Data == "h2" {
					for next := n.NextSibling; next != nil; next = next.NextSibling {
						if next.Type == html.ElementNode && (next.Data == "h1" || next.Data == "h2" || next.Data == "section") {
							break
						}
						_ = html.Render(&rendered, next)
					}
				}
				block = rendered.String()
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(root)
	return block, recipe
}

// JSONAgent returns the agent section or, when the twin only publishes agent
// metadata, its instructions field.
func JSONAgent(body string) (string, map[string]string) {
	var data struct {
		Agents   map[string]string `json:"agents"`
		Sections []struct {
			ID       string `json:"id"`
			Markdown string `json:"markdown"`
		} `json:"sections"`
	}
	if json.Unmarshal([]byte(body), &data) != nil {
		return "", nil
	}
	for _, section := range data.Sections {
		if section.ID == "agents" && section.Markdown != "" {
			return section.Markdown, data.Agents
		}
	}
	return data.Agents["instructions"], data.Agents
}

func ResponseAgent(body, contentType string) string {
	switch {
	case strings.Contains(contentType, "json"):
		block, _ := JSONAgent(body)
		return block
	case strings.Contains(contentType, "html"):
		block, _ := HTMLAgent(body)
		return block
	default:
		block, _ := MarkdownAgent(body)
		return block
	}
}
