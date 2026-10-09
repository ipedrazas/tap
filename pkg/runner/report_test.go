package runner

import (
	"strings"
	"testing"
)

const runOutput = `{"time":"2026-10-09T10:00:00Z","msg":"tool call"}
PASS echo_text / echoes the text (12ms)
PASS echo_text / rate (EUR) is kept (3ms)
SKIP current_time / live clock: needs the mock egress proxy
FAIL forecast / bad city (/tests/forecast.test.yaml): expected ok=false

2 passed, 1 failed, 1 skipped
`

func TestParseReport(t *testing.T) {
	rep, err := ParseReport([]byte(runOutput))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed != 2 || rep.Failed != 1 || rep.Skipped != 1 || len(rep.Cases) != 4 {
		t.Fatalf("got %+v", rep)
	}
	if c := rep.Cases[1]; c.Tool != "echo_text" || c.Case != "rate (EUR) is kept" || c.Result != "pass" {
		t.Fatalf("case 1: %+v", c)
	}
	if c := rep.Cases[3]; c.Case != "bad city" || c.Result != "fail" {
		t.Fatalf("case 3: %+v", c)
	}
	if rep.OK() {
		t.Fatal("a run with a failure is not OK")
	}
}

func TestParseReportRejectsTruncatedOrMissingSummary(t *testing.T) {
	// A log that lost its FAIL line must not read as a clean pass.
	truncated := strings.Replace(runOutput, "FAIL forecast / bad city (/tests/forecast.test.yaml): expected ok=false\n", "", 1)
	if _, err := ParseReport([]byte(truncated)); err == nil {
		t.Fatal("expected a count mismatch error")
	}
	if _, err := ParseReport([]byte("PASS a / b (1ms)\n")); err == nil {
		t.Fatal("expected an error without a summary line")
	}
}

func TestReportOK(t *testing.T) {
	if (Report{}).OK() {
		t.Fatal("an empty run is not OK")
	}
	if !(Report{Passed: 1, Skipped: 2}).OK() {
		t.Fatal("passes with skips are OK")
	}
}
