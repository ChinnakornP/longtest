package security

import (
	"crypto/rand"
	"encoding/hex"
)

// NonceBytes is the entropy behind a frame nonce. 128 bits is far more than a
// guessing attack needs to fail, and the nonce is short enough to stay
// readable in a prompt a human is debugging.
const NonceBytes = 16

// NewNonce mints the per-run frame identifier used by [Wrap].
//
// One nonce per run, not per block: a run's prompts are read together, and
// reusing it lets a reader confirm that two blocks came from the same run.
// It must never be derived from anything the page can observe.
func NewNonce() string {
	var b [NonceBytes]byte
	// crypto/rand.Read is documented never to fail as of Go 1.24; it panics
	// internally on a broken system RNG rather than returning an error.
	rand.Read(b[:]) //nolint:errcheck // cannot fail; see comment
	return hex.EncodeToString(b[:])
}
