package gemini

import (
	"regexp"

	"github.com/bojackduy/bridgecore"
)

var fenceRe = regexp.MustCompile("(?s)```.*?```")

// BridgeCall is the shared protocol type (re-exported).
type BridgeCall = bridgecore.BridgeCall

func ParseBridgeCall(payload string) (*BridgeCall, error) {
	return bridgecore.ParseBridgeCall(payload)
}
