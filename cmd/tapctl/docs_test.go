package main

import (
	"flag"
	"os"
	"testing"
)

var updateDocs = flag.Bool("update", false, "rewrite docs/cli.md")

// docs/cli.md is generated from the command table; it must not drift.
func TestCLIDocsUpToDate(t *testing.T) {
	got := markdown()
	const path = "../../docs/cli.md"
	if *updateDocs {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatal("docs/cli.md is stale; run `task docs:cli`")
	}
}

func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range commands {
		if c.summary == "" || c.group == "" {
			t.Errorf("%s: missing summary or group", c.name)
		}
	}
}
