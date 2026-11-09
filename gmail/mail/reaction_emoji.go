package mail

import (
	_ "embed"
	"strings"
)

// Exact fully-qualified Emoji 16.0 sequences, derived from Unicode's
// emoji-test.txt; see emoji/README.md for provenance, regeneration and license.
// Membership, not rune count or grapheme count, validates a single emoji.
//
//go:embed emoji/fully-qualified-16.0.txt
var reactionEmojiData string

var reactionEmoji = func() map[string]bool {
	m := make(map[string]bool, 3781)
	for s := range strings.SplitSeq(strings.TrimSuffix(reactionEmojiData, "\n"), "\n") {
		m[s] = true
	}
	return m
}()
