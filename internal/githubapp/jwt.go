package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const (
	// jwtLifetime is how long a signed app JWT claims to be valid. GitHub
	// refuses anything over ten minutes, so this leaves a minute of headroom
	// under that ceiling. The JWT is never cached — it is minted for one
	// call to /app/installations or one access-token exchange and thrown
	// away — so a short life costs nothing.
	jwtLifetime = 9 * time.Minute

	// jwtBackdate shifts iat one minute into the past. GitHub rejects a JWT
	// whose iat is in the future by its clock, and a container whose clock
	// runs a few seconds fast is otherwise indistinguishable from a forged
	// token. GitHub's own documentation recommends exactly this.
	jwtBackdate = time.Minute
)

// signJWT returns the RS256 JSON Web Token that authenticates the App
// itself — the credential GitHub calls a "JWT" as opposed to an
// installation token, and the only one that may call /app/... endpoints.
//
// Hand-rolled rather than pulled from a library: the whole of it is two
// base64url-encoded JSON objects and one PKCS#1 v1.5 signature over their
// concatenation, all of which the standard library already has. A JWT
// dependency would carry parsing, verification, and a dozen algorithms this
// process never signs with or accepts.
func signJWT(key *rsa.PrivateKey, appID string, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("githubapp: encode JWT header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", fmt.Errorf("githubapp: encode JWT claims: %w", err)
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: sign JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(signature), nil
}
