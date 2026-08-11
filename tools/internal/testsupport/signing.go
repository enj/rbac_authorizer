package testsupport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"math"
	"os"
	"path/filepath"
)

// SigningKey is an ed25519 key pair written to disk in OpenSSH format, together
// with the fingerprint git reports for commits signed with it.
type SigningKey struct {
	PrivatePath string
	PublicPath  string
	Algorithm   string
	Blob        string
	Fingerprint string
	Comment     string
}

// PublicLine renders the public key file contents without a trailing newline.
func (k SigningKey) PublicLine() string {
	return k.Algorithm + " " + k.Blob + " " + k.Comment
}

// NewSigningKey generates an OpenSSH ed25519 key pair in a temporary directory.
// The key is generated in process so fixtures do not depend on an external key
// generator, while git still uses the real OpenSSH signing path.
func NewSigningKey(tb TB) SigningKey {
	tb.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatalf("generate signing key: %v", err)
	}
	const algorithm = "ssh-ed25519"
	const comment = "soapbox-fixture@example.com"

	blob := sshString(tb, algorithm)
	blob = append(blob, sshString(tb, string(public))...)
	sum := sha256.Sum256(blob)

	dir := tb.TempDir()
	key := SigningKey{
		PrivatePath: filepath.Join(dir, "id_ed25519"),
		PublicPath:  filepath.Join(dir, "id_ed25519.pub"),
		Algorithm:   algorithm,
		Blob:        base64.StdEncoding.EncodeToString(blob),
		Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
		Comment:     comment,
	}
	if err := os.WriteFile(key.PublicPath, []byte(key.PublicLine()+"\n"), 0o600); err != nil {
		tb.Fatalf("write public key: %v", err)
	}
	// OpenSSH refuses to use a private key that other users can read.
	if err := os.WriteFile(key.PrivatePath, opensshPrivateKey(tb, blob, private, comment), 0o600); err != nil {
		tb.Fatalf("write private key: %v", err)
	}
	return key
}

// EnableSSHSigning configures SSH commit signing with key and authorizes it for
// the given allowed signers file contents.
func (r *Repo) EnableSSHSigning(ctx context.Context, tb TB, key SigningKey, allowedSigners string) {
	tb.Helper()
	signers := filepath.Join(r.Dir, "allowed_signers")
	if err := os.WriteFile(signers, []byte(allowedSigners+" "+key.PublicLine()+"\n"), 0o600); err != nil {
		tb.Fatalf("write allowed signers: %v", err)
	}
	r.SetConfig(ctx, tb, "gpg.format", "ssh")
	r.SetConfig(ctx, tb, "user.signingkey", key.PublicPath)
	r.SetConfig(ctx, tb, "commit.gpgsign", "true")
	r.SetConfig(ctx, tb, "tag.gpgsign", "true")
	r.SetConfig(ctx, tb, "gpg.ssh.allowedsignersfile", signers)
}

// sshString encodes one length prefixed SSH wire string.
func sshString(tb TB, value string) []byte {
	tb.Helper()
	length := len(value)
	if length > math.MaxInt32 {
		tb.Fatalf("ssh string of %d bytes does not fit a length prefix", length)
		return nil
	}
	out := make([]byte, 4, 4+length)
	binary.BigEndian.PutUint32(out, uint32(length))
	return append(out, value...)
}

// opensshPrivateKey encodes an unencrypted OpenSSH private key file.
func opensshPrivateKey(tb TB, publicBlob []byte, private ed25519.PrivateKey, comment string) []byte {
	tb.Helper()
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		tb.Fatalf("generated key is not an ed25519 key")
	}

	var inner []byte
	check := make([]byte, 4)
	binary.BigEndian.PutUint32(check, 0x5061636b)
	inner = append(inner, check...)
	inner = append(inner, check...)
	inner = append(inner, sshString(tb, "ssh-ed25519")...)
	inner = append(inner, sshString(tb, string(public))...)
	inner = append(inner, sshString(tb, string(private))...)
	inner = append(inner, sshString(tb, comment)...)
	for i := byte(1); len(inner)%8 != 0; i++ {
		inner = append(inner, i)
	}

	var body []byte
	body = append(body, "openssh-key-v1\x00"...)
	body = append(body, sshString(tb, "none")...)
	body = append(body, sshString(tb, "none")...)
	body = append(body, sshString(tb, "")...)
	count := make([]byte, 4)
	binary.BigEndian.PutUint32(count, 1)
	body = append(body, count...)
	body = append(body, sshString(tb, string(publicBlob))...)
	body = append(body, sshString(tb, string(inner))...)

	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: body})
}
