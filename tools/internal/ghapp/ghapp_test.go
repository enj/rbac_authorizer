package ghapp_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/ghapp"
)

// The App identity every test uses.
const (
	testAppID          = 42
	testInstallationID = 7
)

// sharedKey is generated once per test binary. RSA key generation is the
// slowest thing in this package by an order of magnitude, and every test that
// needs a valid key needs the same one.
var (
	sharedKeyOnce sync.Once
	sharedKey     *rsa.PrivateKey
	sharedKeyErr  error
)

// testKey reports the shared 2048 bit RSA key.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	sharedKeyOnce.Do(func() { sharedKey, sharedKeyErr = rsa.GenerateKey(rand.Reader, 2048) })
	if sharedKeyErr != nil {
		t.Fatalf("generate test key: %v", sharedKeyErr)
	}
	return sharedKey
}

// pkcs1PEM encodes an RSA key the way GitHub hands one out.
func pkcs1PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// pkcs8PEM encodes any key in the modern wrapper.
func pkcs8PEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// testConfig is a valid configuration pointed at no server.
func testConfig(t *testing.T) ghapp.Config {
	t.Helper()
	return ghapp.Config{
		AppID:          testAppID,
		InstallationID: testInstallationID,
		PrivateKeyPEM:  pkcs1PEM(t, testKey(t)),
		Repositories:   []string{"enj/rbac_authorizer"},
		Permissions:    map[string]string{"contents": "write"},
	}
}

func TestNewAcceptsBothKeyEncodings(t *testing.T) {
	t.Parallel()

	key := testKey(t)
	tests := []struct {
		name    string
		encoded []byte
	}{
		{name: "pkcs1", encoded: pkcs1PEM(t, key)},
		{name: "pkcs8", encoded: pkcs8PEM(t, key)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t)
			cfg.PrivateKeyPEM = test.encoded
			if _, err := ghapp.New(cfg); err != nil {
				t.Fatalf("New with a %s key: %v", test.name, err)
			}
		})
	}
}

func TestNewRefusesUnusableKeys(t *testing.T) {
	t.Parallel()

	key := testKey(t)
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate weak key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	ecDER, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshal ec key: %v", err)
	}

	tests := []struct {
		name    string
		encoded []byte
		wantErr error
	}{
		{name: "no key at all", encoded: nil, wantErr: ghapp.ErrPrivateKeyMalformed},
		{name: "whitespace", encoded: []byte("   \n\t "), wantErr: ghapp.ErrPrivateKeyMalformed},
		{name: "garbage", encoded: []byte("this is not a key"), wantErr: ghapp.ErrPrivateKeyMalformed},
		{
			name:    "a PEM block that is not a key",
			encoded: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}),
			wantErr: ghapp.ErrPrivateKeyMalformed,
		},
		{
			name:    "a truncated key body",
			encoded: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
			wantErr: ghapp.ErrPrivateKeyMalformed,
		},
		{
			name:    "two keys in one file",
			encoded: append(pkcs1PEM(t, key), pkcs1PEM(t, key)...),
			wantErr: ghapp.ErrPrivateKeyMalformed,
		},
		{
			name: "a legacy encrypted key",
			encoded: pem.EncodeToMemory(&pem.Block{
				Type:    "RSA PRIVATE KEY",
				Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,0123456789ABCDEF0123456789ABCDEF"},
				Bytes:   []byte("ciphertext"),
			}),
			wantErr: ghapp.ErrPrivateKeyEncrypted,
		},
		{
			name:    "a pkcs8 encrypted key",
			encoded: pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("ciphertext")}),
			wantErr: ghapp.ErrPrivateKeyEncrypted,
		},
		{
			name:    "an ec key",
			encoded: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER}),
			wantErr: ghapp.ErrPrivateKeyNotRSA,
		},
		{name: "a pkcs8 ec key", encoded: pkcs8PEM(t, ecKey), wantErr: ghapp.ErrPrivateKeyNotRSA},
		{name: "an ed25519 key", encoded: pkcs8PEM(t, edKey), wantErr: ghapp.ErrPrivateKeyNotRSA},
		{name: "a 1024 bit rsa key", encoded: pkcs1PEM(t, weak), wantErr: ghapp.ErrPrivateKeyTooWeak},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t)
			cfg.PrivateKeyPEM = test.encoded
			_, err := ghapp.New(cfg)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			// A key file that failed to parse is still a key file.
			if body := strings.TrimSpace(string(test.encoded)); body != "" && strings.Contains(err.Error(), body) {
				t.Fatalf("error %q echoes the key material", err)
			}
		})
	}
}

func TestNewValidatesTheInstallation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		adjust          func(*ghapp.Config)
		wantErrContains string
	}{
		{
			name:            "an app id is required",
			adjust:          func(c *ghapp.Config) { c.AppID = 0 },
			wantErrContains: "app id 0 must be positive",
		},
		{
			name:            "a negative app id is refused",
			adjust:          func(c *ghapp.Config) { c.AppID = -1 },
			wantErrContains: "app id -1 must be positive",
		},
		{
			name:            "an installation id is required",
			adjust:          func(c *ghapp.Config) { c.InstallationID = 0 },
			wantErrContains: "installation id 0 must be positive",
		},
		{
			name:            "a repository must name its owner",
			adjust:          func(c *ghapp.Config) { c.Repositories = []string{"rbac_authorizer"} },
			wantErrContains: "must be owner/name",
		},
		{
			name:            "a repository must not carry a path",
			adjust:          func(c *ghapp.Config) { c.Repositories = []string{"enj/a/b"} },
			wantErrContains: "must be owner/name",
		},
		{
			name:            "two owners cannot share one installation",
			adjust:          func(c *ghapp.Config) { c.Repositories = []string{"enj/a", "other/b"} },
			wantErrContains: "name two owners",
		},
		{
			name:            "a repeated repository is refused",
			adjust:          func(c *ghapp.Config) { c.Repositories = []string{"enj/a", "enj/a"} },
			wantErrContains: "listed twice",
		},
		{
			name:            "a repeated repository in another case is still one repository",
			adjust:          func(c *ghapp.Config) { c.Repositories = []string{"enj/a", "ENJ/A"} },
			wantErrContains: "listed twice",
		},
		{
			name:            "a repository name is held to GitHub's character set",
			adjust:          func(c *ghapp.Config) { c.Repositories = []string{"enj/rbac authorizer"} },
			wantErrContains: "may only contain",
		},
		{
			name:            "an unknown permission level is refused",
			adjust:          func(c *ghapp.Config) { c.Permissions = map[string]string{"contents": "full"} },
			wantErrContains: "must be read, write, or admin",
		},
		{
			name:            "asking for no permission is refused",
			adjust:          func(c *ghapp.Config) { c.Permissions = map[string]string{"contents": "none"} },
			wantErrContains: "not none",
		},
		{
			name:            "a renewal margin past the bound is refused",
			adjust:          func(c *ghapp.Config) { c.RenewBefore = time.Hour },
			wantErrContains: "renewal margin",
		},
		{
			name:            "a negative renewal margin is refused",
			adjust:          func(c *ghapp.Config) { c.RenewBefore = -time.Second },
			wantErrContains: "renewal margin",
		},
		{
			name:            "a plaintext base URL is refused",
			adjust:          func(c *ghapp.Config) { c.BaseURL = "http://api.github.com" },
			wantErrContains: "must use https",
		},
		{
			name:            "a base URL carrying a credential is refused",
			adjust:          func(c *ghapp.Config) { c.BaseURL = "https://x:y@api.github.com" },
			wantErrContains: "must not embed credentials",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := testConfig(t)
			test.adjust(&cfg)
			_, err := ghapp.New(cfg)
			if err == nil {
				t.Fatal("the configuration was accepted, want an error")
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("error %q does not contain %q", err, test.wantErrContains)
			}
		})
	}
}

func TestNewAcceptsAnInstallationWithNoNarrowing(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.Repositories = nil
	cfg.Permissions = nil
	app, err := ghapp.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := app.BaseURL(); got != "https://api.github.com" {
		t.Fatalf("base URL = %q, want the production root", got)
	}
}

// jwtClaims is the claim set GitHub requires, decoded from a minted token.
type jwtClaims struct {
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// splitJWT decodes a compact JWT and verifies its RS256 signature.
func splitJWT(t *testing.T, token string, public *rsa.PublicKey) (header string, claims jwtClaims) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	for i, part := range parts {
		if strings.ContainsAny(part, "=+/") {
			t.Fatalf("segment %d is not unpadded base64url: %q", i, part)
		}
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verify signature: %v", err)
	}
	return string(rawHeader), claims
}
