package gitcli

import (
	"strings"
	"testing"
)

// TestParseCommitRecordsFailsClosed pins the guard that keeps a desynchronised
// stream from being read as a shorter one.
//
// Git refuses to create a commit whose message holds a null byte, so a stream
// that does not divide evenly into records means the objects themselves are
// corrupt. Attributing the surviving fields to the wrong commit would be worse
// than reporting nothing, because the result would look like a valid answer.
func TestParseCommitRecordsFailsClosed(t *testing.T) {
	const (
		sha1   = "0123456789abcdef0123456789abcdef01234567"
		sha256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	// record renders one commit exactly as git -z frames it: the fields, then a
	// terminating null byte.
	record := func(name, subject string) string {
		fields := make([]string, commitFieldCount)
		fields[0] = name
		fields[commitSubjectField] = subject
		return strings.Join(fields, "\x00") + "\x00"
	}

	tests := []struct {
		name string
		out  string
		want int
	}{
		{name: "empty output"},
		{name: "only the trailing newline", out: "\n"},
		{name: "one record", out: record(sha1, "feat: one") + "\n", want: 1},
		{name: "no trailing newline", out: record(sha1, "feat: one"), want: 1},
		{name: "two records", out: record(sha1, "one") + record(sha256, "two") + "\n", want: 2},
		{name: "sha256 object names", out: record(sha256, "feat: one") + "\n", want: 1},
		{name: "a field short", out: strings.TrimSuffix(record(sha1, "one"), "\x00\x00") + "\x00", want: -1},
		{name: "an extra field", out: record(sha1, "one") + "extra\x00", want: -1},
		{name: "record does not start with an object name", out: record("not-an-object-name", "one") + "\n", want: -1},
		{name: "abbreviated object name", out: record(sha1[:12], "one") + "\n", want: -1},
		{name: "upper case object name", out: record(strings.ToUpper(sha256), "one") + "\n", want: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commits, err := parseCommitRecords(test.out)
			if test.want < 0 {
				if err == nil {
					t.Fatalf("malformed stream was accepted as %d commits", len(commits))
				}
				if commits != nil {
					t.Fatalf("rejected stream returned %d commits", len(commits))
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(commits) != test.want {
				t.Fatalf("got %d commits, want %d", len(commits), test.want)
			}
		})
	}
}

// TestCommitFormatsShareOneRecordShape pins the reason CommitInfo and CommitLog
// can share a parser: the batch form leaves the signature slots empty rather
// than leaving them out, so both produce the same number of fields.
func TestCommitFormatsShareOneRecordShape(t *testing.T) {
	tests := []struct {
		name   string
		format string
	}{
		{name: "signed", format: commitFormat},
		{name: "unsigned", format: commitFormatUnsigned},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// The separators are the field boundaries, so one fewer than the
			// field count is the whole contract.
			if got := strings.Count(test.format, "%x00"); got != commitFieldCount-1 {
				t.Fatalf("format has %d separators, want %d", got, commitFieldCount-1)
			}
		})
	}
	if strings.Contains(commitFormatUnsigned, "%G") {
		t.Fatal("the unsigned format still asks git to verify a signature")
	}
	if !strings.Contains(commitFormat, "%G?") {
		t.Fatal("the signed format does not ask for a signature status")
	}
}
