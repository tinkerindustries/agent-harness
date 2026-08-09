package session

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns a random hex identifier prefixed for readability. It does
// not depend on an external UUID package: sixteen bytes of crypto/rand is
// enough entropy for a session identifier and needs nothing beyond the
// standard library.
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("session: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
