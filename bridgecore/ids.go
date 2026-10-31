package bridgecore

import (
	"crypto/sha256"
	"fmt"
)

// StableCallID builds a deterministic call ID from the answer text and index.
func StableCallID(answer string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", answer, index)))
	return fmt.Sprintf("call_%x", sum[:12])
}
