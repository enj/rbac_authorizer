package replay

import (
	"errors"
	"strings"
	"testing"
)

// canonical is a profile hash of exactly the form the engine's plan reports:
// the sha256 algorithm and 64 lower case hexadecimal characters.
const canonical = "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// TestValidateProfileHash pins the exact form an epoch may be identified by.
//
// The looser rule this replaced accepted any lower case string without spaces,
// which meant a value that is not a digest at all, or a digest of a different
// algorithm, could be recorded as the identity of an epoch and compared against
// a real one. Since that identity is what decides whether published history may
// be extended or has to start a new epoch, the comparison has to be between two
// things of the same kind.
func TestValidateProfileHash(t *testing.T) {
	t.Parallel()
	digits := strings.Repeat("ab", profileHashDigits/2)
	for _, test := range []struct {
		name string
		hash string
		// message is a fragment of the expected refusal. Empty accepts the hash.
		message string
	}{
		{name: "canonical", hash: canonical},
		{name: "all digits", hash: profileHashPrefix + strings.Repeat("0", profileHashDigits)},
		{name: "all letters", hash: profileHashPrefix + strings.Repeat("f", profileHashDigits)},
		{name: "empty", message: "a profile hash is required"},
		{name: "a bare lower case word", hash: "x", message: `must begin with "sha256:"`},
		{name: "a bare digest", hash: digits, message: `must begin with "sha256:"`},
		{name: "another algorithm", hash: "sha1:" + digits, message: `must begin with "sha256:"`},
		{name: "a longer algorithm name", hash: "sha512:" + digits, message: `must begin with "sha256:"`},
		{name: "an upper case algorithm", hash: "SHA256:" + digits, message: `must begin with "sha256:"`},
		{name: "no separator", hash: "sha256" + digits, message: `must begin with "sha256:"`},
		{name: "no digest at all", hash: profileHashPrefix, message: "carries 0 digest characters, want 64"},
		{
			name:    "a digest one character short",
			hash:    profileHashPrefix + digits[:profileHashDigits-1],
			message: "carries 63 digest characters, want 64",
		},
		{
			name:    "a digest one character long",
			hash:    profileHashPrefix + digits + "a",
			message: "carries 65 digest characters, want 64",
		},
		{
			name:    "an abbreviated digest",
			hash:    profileHashPrefix + digits[:12],
			message: "carries 12 digest characters, want 64",
		},
		{
			name:    "an upper case digest",
			hash:    profileHashPrefix + strings.ToUpper(digits),
			message: "must be lower case hexadecimal",
		},
		{
			name:    "a digest carrying a non hexadecimal letter",
			hash:    profileHashPrefix + "g" + digits[1:],
			message: "must be lower case hexadecimal",
		},
		{
			name:    "a digest carrying punctuation",
			hash:    profileHashPrefix + "!" + digits[1:],
			message: "must be lower case hexadecimal",
		},
		{
			name:    "a digest carrying a space",
			hash:    profileHashPrefix + " " + digits[1:],
			message: "must be lower case hexadecimal",
		},
		{
			// The length is counted in bytes, so a multi byte rune is refused for
			// its length before its characters are judged. Either refusal is
			// correct; the point is that neither accepts it.
			name:    "a digest carrying a multi byte rune",
			hash:    profileHashPrefix + "é" + digits[1:],
			message: "carries 65 digest characters, want 64",
		},
		{
			name:    "surrounding whitespace",
			hash:    " " + canonical + " ",
			message: `must begin with "sha256:"`,
		},
		{
			name:    "a trailing newline",
			hash:    canonical + "\n",
			message: "carries 65 digest characters, want 64",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateProfileHash(test.hash)
			if test.message == "" {
				if err != nil {
					t.Fatalf("hash %q was refused with %v", test.hash, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("hash %q was accepted", test.hash)
			}
			if !errors.Is(err, ErrProfileHash) {
				t.Fatalf("hash %q was refused with %v, want %v", test.hash, err, ErrProfileHash)
			}
			if !strings.Contains(err.Error(), test.message) {
				t.Fatalf("hash %q was refused with %v, want a refusal mentioning %q", test.hash, err, test.message)
			}
		})
	}
}

// TestProfileHashConstantsMatchThePlan checks the constants against the form the
// engine's plan actually reports, which is "sha256:" and a hex encoded sha256
// sum. A change to either constant that was not also a change to the producer
// would refuse every real profile hash, so the relationship is pinned here.
func TestProfileHashConstantsMatchThePlan(t *testing.T) {
	t.Parallel()
	// sha256 produces 32 bytes, and hex encoding renders each as two characters.
	if want := 2 * 32; profileHashDigits != want {
		t.Fatalf("a profile hash carries %d digest characters, want %d", profileHashDigits, want)
	}
	if profileHashPrefix != "sha256:" {
		t.Fatalf("a profile hash names %q, want the algorithm the plan reports", profileHashPrefix)
	}
}
