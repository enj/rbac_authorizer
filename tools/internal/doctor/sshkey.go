package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// publicKey is a parsed OpenSSH public key.
type publicKey struct {
	Algorithm   string
	Blob        string
	Fingerprint string
}

// loadPublicKey reads an OpenSSH public key file and computes the SHA256
// fingerprint git reports for a signed commit. The fingerprint is computed in
// process so the doctor does not need a second external tool to verify that
// HEAD was signed with the policy key.
func loadPublicKey(ctx context.Context, path string) (publicKey, error) {
	if err := ctx.Err(); err != nil {
		return publicKey{}, err
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return publicKey{}, fmt.Errorf("read public key: %w", err)
	}
	return parsePublicKey(string(data))
}

// parsePublicKey parses the first key entry of an OpenSSH public key file.
func parsePublicKey(contents string) (publicKey, error) {
	for line := range strings.SplitSeq(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return publicKey{}, fmt.Errorf("public key line %q is not <algorithm> <base64>", line)
		}
		if !isKeyAlgorithm(fields[0]) {
			return publicKey{}, fmt.Errorf("unsupported public key algorithm %q", fields[0])
		}
		blob, err := base64.StdEncoding.DecodeString(fields[1])
		if err != nil {
			return publicKey{}, fmt.Errorf("decode public key: %w", err)
		}
		sum := sha256.Sum256(blob)
		return publicKey{
			Algorithm:   fields[0],
			Blob:        fields[1],
			Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
		}, nil
	}
	return publicKey{}, fmt.Errorf("public key file contains no key")
}

// isKeyAlgorithm reports whether field names an OpenSSH public key algorithm.
func isKeyAlgorithm(field string) bool {
	for _, prefix := range []string{"ssh-", "ecdsa-", "sk-"} {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}
