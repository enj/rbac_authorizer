package ghapp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The shape of every JWT this package mints.
//
// The issued-at claim is backdated because GitHub rejects a token whose iat is
// in its own future, and the two clocks are not the same clock. The lifetime is
// short because a JWT is a bearer credential for the whole App rather than one
// installation: it can mint tokens for every repository the App is installed
// on, so it is minted for one request and never stored.
//
// GitHub refuses a JWT whose exp is more than ten minutes past its iat. The
// backdate counts against that budget, so the lifetime is derived from the
// ceiling rather than written beside it: the two cannot drift apart into a
// token GitHub rejects, and the spare minute absorbs a slow request.
const (
	jwtMaxAge   = 10 * time.Minute
	jwtBackdate = 60 * time.Second
	jwtLifetime = jwtMaxAge - jwtBackdate - time.Minute
)

// jwtHeader is the JOSE header of an RS256 token.
type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

// jwtClaims is the claim set GitHub requires of an App JWT. The field order is
// the encoded order, so two mints at the same instant produce the same bytes.
type jwtClaims struct {
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// appJWT mints one GitHub App JWT valid at now.
//
// The signature is RSASSA-PKCS1-v1_5, which is deterministic: the same key, the
// same claims, and the same clock produce the same token every time, so a test
// can assert the exact bytes rather than that something parses.
func appJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	if key == nil {
		return "", fmt.Errorf("mint app jwt: no private key")
	}
	issuedAt := now.Add(-jwtBackdate).UTC()
	expiresAt := now.Add(jwtLifetime).UTC()
	header, err := encodeSegment(jwtHeader{Algorithm: "RS256", Type: "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := encodeSegment(jwtClaims{
		Issuer:    strconv.FormatInt(appID, 10),
		IssuedAt:  issuedAt.Unix(),
		ExpiresAt: expiresAt.Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	// The random source is unused for PKCS#1 v1.5 signing and is passed as nil
	// so the result stays reproducible.
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("mint app jwt: sign: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// encodeSegment renders one JWT segment as unpadded base64url JSON.
func encodeSegment(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("mint app jwt: encode %T: %w", value, err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
