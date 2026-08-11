package doctor

import (
	"strings"
	"testing"
)

func TestParsePublicKey(t *testing.T) {
	// The vector is a real OpenSSH ed25519 public key and the fingerprint
	// ssh-keygen -lf reports for it, so the in process fingerprint must match
	// what git prints for a signed commit.
	const (
		vectorKey         = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBGCSSRl8xJyPumnSt8gZI/pyuFchtZCrv5b3+8mT1zx vector@example.com"
		vectorFingerprint = "SHA256:cypmBC1kGwq8kRhRatVBjypnUDgPq8Es6Mzo3j+a3/4"
		vectorBlob        = "AAAAC3NzaC1lZDI1NTE5AAAAIBGCSSRl8xJyPumnSt8gZI/pyuFchtZCrv5b3+8mT1zx"
	)

	tests := []struct {
		name            string
		contents        string
		wantAlgorithm   string
		wantFingerprint string
		wantErr         string
	}{
		{
			name:            "ed25519 key",
			contents:        vectorKey + "\n",
			wantAlgorithm:   "ssh-ed25519",
			wantFingerprint: vectorFingerprint,
		},
		{
			name:            "key without a comment",
			contents:        "ssh-ed25519 " + vectorBlob + "\n",
			wantAlgorithm:   "ssh-ed25519",
			wantFingerprint: vectorFingerprint,
		},
		{
			name:            "leading comment and blank lines",
			contents:        "# soapbox signing key\n\n" + vectorKey + "\n",
			wantAlgorithm:   "ssh-ed25519",
			wantFingerprint: vectorFingerprint,
		},
		{name: "empty file", contents: "", wantErr: "contains no key"},
		{name: "only comments", contents: "# nothing\n", wantErr: "contains no key"},
		{name: "single field", contents: "ssh-ed25519\n", wantErr: "not <algorithm> <base64>"},
		{name: "malformed base64", contents: "ssh-ed25519 not-base-64!!\n", wantErr: "decode public key"},
		{name: "private key", contents: "-----BEGIN OPENSSH PRIVATE KEY-----\n", wantErr: "unsupported public key algorithm"},
		{name: "unsupported algorithm", contents: "pgp-rsa " + vectorBlob + "\n", wantErr: "unsupported public key algorithm"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, err := parsePublicKey(test.contents)
			switch {
			case test.wantErr == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if key.Algorithm != test.wantAlgorithm {
					t.Fatalf("algorithm = %q, want %q", key.Algorithm, test.wantAlgorithm)
				}
				if key.Fingerprint != test.wantFingerprint {
					t.Fatalf("fingerprint = %q, want %q", key.Fingerprint, test.wantFingerprint)
				}
			case err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestDescribeSignatureStatus(t *testing.T) {
	tests := map[string]string{
		"G": "good signature",
		"B": "bad signature",
		"U": "unknown validity",
		"X": "expired",
		"Y": "expired key",
		"R": "revoked key",
		"E": "could not be checked",
		"N": "no signature",
		"?": "unknown status",
	}
	for code, want := range tests {
		t.Run(code, func(t *testing.T) {
			if got := describeSignatureStatus(code); !strings.Contains(got, want) {
				t.Fatalf("describeSignatureStatus(%q) = %q, want it to contain %q", code, got, want)
			}
		})
	}
}

func TestWritableDetail(t *testing.T) {
	existing := t.TempDir()

	tests := []struct {
		name    string
		path    string
		wantOK  bool
		wantMsg string
	}{
		{name: "existing directory", path: existing, wantOK: true, wantMsg: "is writable"},
		{name: "missing child", path: existing + "/pkg/mod", wantOK: true, wantMsg: "will be created under writable"},
		{name: "unset", path: "", wantMsg: "not set"},
		{name: "disabled cache", path: "off", wantMsg: "disabled"},
		{name: "relative path", path: "cache", wantMsg: "is not an absolute path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail, ok := writableDetail(t.Context(), test.path)
			if ok != test.wantOK {
				t.Fatalf("writableDetail(%q) = %t, want %t (%s)", test.path, ok, test.wantOK, detail)
			}
			if !strings.Contains(detail, test.wantMsg) {
				t.Fatalf("detail %q does not contain %q", detail, test.wantMsg)
			}
		})
	}
}
