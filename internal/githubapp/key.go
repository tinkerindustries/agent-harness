package githubapp

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// pemBody matches the base64 payload between a private key's BEGIN and END
// markers, whatever label the marker carries — GitHub hands out a PKCS#1
// key ("RSA PRIVATE KEY"), and a key an operator has converted is PKCS#8
// ("PRIVATE KEY"). (?s) lets the body span lines; the lazy group stops at
// the first END so a file holding more than one block yields the first.
var pemBody = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----(.*?)-----END [A-Z ]*PRIVATE KEY-----`)

// ParsePrivateKey reads the RSA key GitHub issued for the App out of the
// text stored in github.app_private_key.
//
// It is deliberately forgiving about whitespace, because of where that text
// comes from: the settings screen types a secret into a single-line
// password field, and a PEM pasted into one arrives with its newlines
// flattened to spaces or dropped altogether. A parser that insisted on the
// canonical 64-column PEM would reject the very thing an operator is most
// likely to paste. So the base64 payload is taken from between the BEGIN
// and END markers, every space and newline in it is discarded, and what is
// left is decoded — which accepts the canonical file, the flattened paste,
// and a bare base64 DER with no markers at all, all through one path.
//
// Both DER encodings GitHub's download page and `openssl pkcs8` produce are
// accepted: PKCS#1 first, PKCS#8 second. A PKCS#8 key holding anything but
// an RSA key is refused by name, because GitHub Apps sign with RS256 and
// nothing else.
func ParsePrivateKey(text string) (*rsa.PrivateKey, error) {
	body := text
	if m := pemBody.FindStringSubmatch(text); m != nil {
		body = m[1]
	}
	body = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, body)
	if body == "" {
		return nil, fmt.Errorf("githubapp: the stored private key is empty")
	}
	der, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("githubapp: the stored private key is not valid base64: %w", err)
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("githubapp: the stored private key parses as neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("githubapp: the stored private key is a %T; a GitHub App signs with RSA", parsed)
	}
	return key, nil
}
