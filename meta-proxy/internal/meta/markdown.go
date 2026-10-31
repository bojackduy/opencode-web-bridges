package meta

import "github.com/bojackduy/bridgecore"

// answerToMarkdown is retained as a thin adapter so existing tests keep
// passing; the canonical implementation lives in bridgecore.
func answerToMarkdown(htmlFrag, plain string) string {
	return bridgecore.AnswerToMarkdown(htmlFrag, plain)
}
