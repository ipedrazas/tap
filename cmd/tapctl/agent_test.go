package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ipedrazas/tap/pkg/inventory"
)

func TestStalePreviews(t *testing.T) {
	now := time.Now()
	agents := []inventory.Agent{
		{Name: "weather-agent", Created: now.Add(-30 * 24 * time.Hour)},
		{Name: "weather-agent-dev", Preview: true, PreviewOf: "weather-agent", Created: now.Add(-9 * 24 * time.Hour)},
		{Name: "hn-agent-dev", Preview: true, PreviewOf: "hn-agent", Created: now.Add(-2 * time.Hour)},
	}
	var buf bytes.Buffer
	if err := stalePreviews(&buf, agents, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 preview(s) older than 7d") || !strings.Contains(out, "weather-agent-dev (9d): task agent:preview:rm AGENT=weather-agent") {
		t.Errorf("footer:\n%s", out)
	}
	if strings.Contains(out, "hn-agent") || strings.Contains(out, "AGENT=weather-agent-dev") {
		t.Errorf("footer names the wrong agents:\n%s", out)
	}
	buf.Reset()
	if err := stalePreviews(&buf, agents[:1], now); err != nil || buf.Len() != 0 {
		t.Errorf("no stale previews should print nothing, got %q", buf.String())
	}
}
