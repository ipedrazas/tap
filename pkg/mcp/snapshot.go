package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ipedrazas/tap/pkg/spec"
)

// Snapshot maps an MCP tool name to its pinned definition, as stored in
// mcp/<server>.tools.json.
type Snapshot map[string]spec.PinnedTool

// Pin selects the allowlisted tools from a live listing.
func Pin(live []Tool, allow []string) (Snapshot, error) {
	byName := map[string]Tool{}
	for _, t := range live {
		byName[t.Name] = t
	}
	snap := Snapshot{}
	var missing []string
	for _, name := range allow {
		t, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		snap[name] = spec.PinnedTool{Description: t.Description, InputSchema: t.InputSchema}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("server does not offer: %s", strings.Join(missing, ", "))
	}
	return snap, nil
}

// Drift lists allowlisted tools whose live input schema differs from the
// pinned one, or that the server no longer offers.
func Drift(pinned Snapshot, live []Tool) []string {
	byName := map[string]Tool{}
	for _, t := range live {
		byName[t.Name] = t
	}
	var drifted []string
	for name, p := range pinned {
		t, ok := byName[name]
		if !ok || !bytes.Equal(Canonical(p.InputSchema), Canonical(t.InputSchema)) {
			drifted = append(drifted, name)
		}
	}
	sort.Strings(drifted)
	return drifted
}

// Canonical re-encodes JSON with sorted keys and no insignificant whitespace.
func Canonical(raw json.RawMessage) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// Shape turns a CallToolResult into what the model sees. Text-only content
// collapses to {"text": ...}; structuredContent wins when present.
func Shape(raw json.RawMessage) (out json.RawMessage, isError bool, errText string, err error) {
	var r struct {
		Content           []map[string]any `json:"content"`
		StructuredContent json.RawMessage  `json:"structuredContent"`
		IsError           bool             `json:"isError"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, false, "", fmt.Errorf("result is not a CallToolResult: %w", err)
	}
	var texts []string
	allText := true
	for _, c := range r.Content {
		if c["type"] == "text" {
			if s, ok := c["text"].(string); ok {
				texts = append(texts, s)
				continue
			}
		}
		allText = false
	}
	joined := strings.Join(texts, "\n")
	if r.IsError {
		if len(joined) > 2000 {
			joined = joined[:2000] + "…"
		}
		return nil, true, joined, nil
	}
	switch {
	case len(r.StructuredContent) > 0 && string(r.StructuredContent) != "null":
		return r.StructuredContent, false, "", nil
	case allText:
		out, err := json.Marshal(map[string]string{"text": joined})
		return out, false, "", err
	default:
		out, err := json.Marshal(map[string]any{"content": r.Content})
		return out, false, "", err
	}
}
