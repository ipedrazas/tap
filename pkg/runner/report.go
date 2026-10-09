package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
)

// Report summarises one fixture run, as read back from `tap-runner test`
// output (the fixture Job's log) for the bundle attestation.
type Report struct {
	Passed  int          `json:"passed"`
	Failed  int          `json:"failed"`
	Skipped int          `json:"skipped"`
	Cases   []CaseReport `json:"cases"`
}

type CaseReport struct {
	Tool   string `json:"tool"`
	Case   string `json:"case"`
	Result string `json:"result"` // pass, fail or skip
}

// OK is the gate the attestation records: something ran and nothing failed.
func (r Report) OK() bool { return r.Failed == 0 && r.Passed > 0 }

// These match the lines printed by cmd/runner's test command.
var (
	caseLine    = regexp.MustCompile(`^(PASS|FAIL|SKIP) (\S+) / (.+?)(?: \(\d+ms\)| \([^)]*\): .*|: .*)?$`)
	summaryLine = regexp.MustCompile(`^(\d+) passed, (\d+) failed, (\d+) skipped$`)
)

// ParseReport reads the case lines and the summary from a fixture run's
// output. Other lines (logs) are ignored. The summary must agree with the
// cases, so a truncated log is an error rather than a smaller pass.
func ParseReport(out []byte) (Report, error) {
	rep := Report{Cases: []CaseReport{}}
	var summary []int
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := summaryLine.FindStringSubmatch(line); m != nil {
			summary = make([]int, 3)
			for i := range summary {
				summary[i], _ = strconv.Atoi(m[i+1])
			}
			continue
		}
		m := caseLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		c := CaseReport{Tool: m[2], Case: m[3]}
		switch m[1] {
		case "PASS":
			rep.Passed++
			c.Result = "pass"
		case "FAIL":
			rep.Failed++
			c.Result = "fail"
		case "SKIP":
			rep.Skipped++
			c.Result = "skip"
		}
		rep.Cases = append(rep.Cases, c)
	}
	if err := sc.Err(); err != nil {
		return Report{}, err
	}
	if summary == nil {
		return Report{}, fmt.Errorf("no fixture summary line in the output")
	}
	if summary[0] != rep.Passed || summary[1] != rep.Failed || summary[2] != rep.Skipped {
		return Report{}, fmt.Errorf("summary says %d passed, %d failed, %d skipped but the output lists %d, %d, %d",
			summary[0], summary[1], summary[2], rep.Passed, rep.Failed, rep.Skipped)
	}
	return rep, nil
}
