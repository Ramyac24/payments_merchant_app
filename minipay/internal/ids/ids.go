// Package ids generates prefixed random identifiers like "pay_3f9a...".
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix + "_" + 24 random hex characters (96 bits of entropy).
func New(prefix string) string {
	return prefix + "_" + Random(12)
}

// Random returns n random bytes, hex-encoded.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
