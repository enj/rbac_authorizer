package doctor

import (
	"fmt"
	"io"
	"strings"
)

// Report is the complete result of one doctor run.
type Report struct {
	Checks []Check
}

// OK reports whether every required check passed.
func (r *Report) OK() bool {
	return len(r.Failures()) == 0
}

// Failures reports the required checks that did not pass.
func (r *Report) Failures() []Check {
	var failed []Check
	for _, check := range r.Checks {
		if check.Status == StatusFail {
			failed = append(failed, check)
		}
	}
	return failed
}

// Counts reports how many checks ended in each status.
func (r *Report) Counts() map[Status]int {
	counts := make(map[Status]int, 4)
	for _, check := range r.Checks {
		counts[check.Status]++
	}
	return counts
}

// Write renders the report as aligned, greppable text in check order.
func (r *Report) Write(w io.Writer) error {
	width := 0
	for _, check := range r.Checks {
		width = max(width, len(check.Name))
	}
	var b strings.Builder
	for _, check := range r.Checks {
		fmt.Fprintf(&b, "%-4s %-*s  %s\n", check.Status, width, check.Name, check.Detail)
	}
	counts := r.Counts()
	fmt.Fprintf(&b, "%d checks: %d passed, %d failed, %d warned, %d skipped\n",
		len(r.Checks), counts[StatusPass], counts[StatusFail], counts[StatusWarn], counts[StatusSkip])
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write doctor report: %w", err)
	}
	return nil
}
