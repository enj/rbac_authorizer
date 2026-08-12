package treebuild

import (
	"errors"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// TestValidateRawDateFollowsGitcli pins that this package states no rule of its
// own about what a raw date is.
//
// It used to. The copy here refused a negative count of seconds while gitcli's
// accepted one, so a pre-1970 upstream author date could be written into a tag
// object and refused for the commit beside it. The rule now has one home and
// this only adds the role and the sentinel.
func TestValidateRawDateFollowsGitcli(t *testing.T) {
	dates := []string{
		"1700000000 +0000",
		"-100000 +0000",
		"-1 -0500",
		"0 +0000",
		"",
		"1700000000",
		"2023-11-14T22:13:20Z",
		"yesterday",
		"1700000000 +053",
	}
	for _, date := range dates {
		t.Run(date, func(t *testing.T) {
			want := gitcli.ValidateRawDate(date)
			got := validateRawDate("author", date)
			if (want == nil) != (got == nil) {
				t.Fatalf("gitcli says %v and this package says %v for %q", want, got, date)
			}
			if got != nil && !errors.Is(got, ErrRawDate) {
				t.Errorf("error %v does not wrap %v", got, ErrRawDate)
			}
		})
	}
}
