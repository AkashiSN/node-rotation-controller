package surge

import (
	"crypto/sha256"
	"encoding/hex"
)

// placeholderNamePrefix is prepended to the old NodeClaim's name to form a
// deterministic placeholder Pod name, so the state machine's create is
// idempotent (a re-create after a crash hits AlreadyExists rather than spawning
// a duplicate).
const placeholderNamePrefix = "noderotation-surge-"

// podNameMax is the DNS-1123 subdomain length limit Kubernetes enforces on Pod
// names. A create that exceeds it fails with Invalid permanently, so the
// derivation below is bounded to never produce a longer name.
const podNameMax = 253

// nameHashLen is the number of hex characters of the full-name hash kept in the
// truncated form — enough to make a collision between two distinct overflowing
// names astronomically unlikely while leaving room for a readable prefix.
const nameHashLen = 16

// PlaceholderName derives the placeholder Pod's name from the old NodeClaim's
// name. The state machine relies on it being deterministic: the create path and
// the later lookup/delete must agree on the name without storing it, and a
// crash-recovery re-create must hit AlreadyExists rather than spawn a duplicate.
//
// Karpenter NodeClaim names are short generated names, so the common path is the
// readable prefix+name form. Should the combined name overflow the 253-char Pod
// name limit (#10 review), it is truncated and a short deterministic hash of the
// FULL claim name is appended — preserving determinism while keeping two
// overflowing names that share a truncation prefix collision-free.
func PlaceholderName(claimName string) string {
	full := placeholderNamePrefix + claimName
	if len(full) <= podNameMax {
		return full
	}
	sum := sha256.Sum256([]byte(claimName))
	hash := hex.EncodeToString(sum[:])[:nameHashLen]
	keep := podNameMax - len(placeholderNamePrefix) - 1 - nameHashLen // 1 for the "-" separator
	return placeholderNamePrefix + claimName[:keep] + "-" + hash
}
