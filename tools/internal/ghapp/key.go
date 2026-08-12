package ghapp

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// minimumKeyBits is the smallest RSA modulus this package accepts. GitHub
// issues 2048 bit App keys, so a shorter one is a key from somewhere else.
const minimumKeyBits = 2048

// Refusals a caller can branch on with errors.Is when a key is rejected.
var (
	// ErrPrivateKeyMalformed reports a key that is not a readable PEM private
	// key, including an empty file and a file holding something else.
	ErrPrivateKeyMalformed = errors.New("the private key is not a readable PEM private key")

	// ErrPrivateKeyEncrypted reports a passphrase protected key. The engine
	// never prompts and never carries a passphrase, so such a key is refused
	// rather than half supported.
	ErrPrivateKeyEncrypted = errors.New("the private key is encrypted")

	// ErrPrivateKeyNotRSA reports a key of the wrong kind. GitHub App JWTs are
	// RS256, so an EC or Ed25519 key could never sign one.
	ErrPrivateKeyNotRSA = errors.New("the private key is not an RSA key")

	// ErrPrivateKeyTooWeak reports an RSA key below minimumKeyBits.
	ErrPrivateKeyTooWeak = errors.New("the RSA private key is too short")
)

// parsePrivateKey reads a GitHub App private key from PEM bytes.
//
// GitHub hands out PKCS#1 keys today and PKCS#8 through some tooling, so both
// are read and everything else is refused. No message this function returns
// ever quotes the input. A private key file that fails to parse is still a
// private key file, and the one thing a caller reliably does with an error is
// write it somewhere that outlives the process.
func parsePrivateKey(encoded []byte) (*rsa.PrivateKey, error) {
	if len(bytes.TrimSpace(encoded)) == 0 {
		return nil, fmt.Errorf("the private key is empty: %w", ErrPrivateKeyMalformed)
	}
	block, rest := pem.Decode(encoded)
	if block == nil {
		return nil, ErrPrivateKeyMalformed
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("the private key holds more than one PEM block: %w", ErrPrivateKeyMalformed)
	}
	if encrypted(block) {
		return nil, ErrPrivateKeyEncrypted
	}

	var parsed any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		return nil, fmt.Errorf("the key is an EC key: %w", ErrPrivateKeyNotRSA)
	default:
		return nil, fmt.Errorf("PEM block %q is not a private key: %w", block.Type, ErrPrivateKeyMalformed)
	}
	if err != nil {
		// The parser's own message is dropped along with the bytes that caused
		// it. What a caller can act on is that this file is not a usable key,
		// and that is what the sentinel says.
		return nil, fmt.Errorf("the %s block does not decode: %w", block.Type, ErrPrivateKeyMalformed)
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is a %T: %w", parsed, ErrPrivateKeyNotRSA)
	}
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf("the RSA key is internally inconsistent: %w", ErrPrivateKeyMalformed)
	}
	if bits := key.N.BitLen(); bits < minimumKeyBits {
		return nil, fmt.Errorf("the RSA key is %d bits, the minimum is %d: %w", bits, minimumKeyBits, ErrPrivateKeyTooWeak)
	}
	return key, nil
}

// encrypted reports whether a PEM block is passphrase protected, in either of
// the two ways a private key file says so: the legacy OpenSSL headers that
// carry the cipher beside the body, and the PKCS#8 encrypted block type.
func encrypted(block *pem.Block) bool {
	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return true
	}
	if _, ok := block.Headers["DEK-Info"]; ok {
		return true
	}
	return strings.Contains(strings.ToUpper(block.Headers["Proc-Type"]), "ENCRYPTED")
}
