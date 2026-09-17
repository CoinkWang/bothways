package state

import (
	"strings"
	"unicode"
)

// Similarity is the Jaccard index of the token sets of two questions. Text is
// lowercased and punctuation dropped; latin words are tokens, and each Han /
// kana / hangul character is its own token so CJK questions compare sensibly.
func Similarity(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func tokens(s string) map[string]bool {
	set := map[string]bool{}
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			set[word.String()] = true
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
			flush()
			set[string(r)] = true
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return set
}
