package githubapp

import (
	"crypto"
	"crypto/sha256"
)

// The two crypto details verifyRS256 needs, kept out of the test file so its
// imports stay about the thing under test.
const cryptoSHA256 = crypto.SHA256

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
