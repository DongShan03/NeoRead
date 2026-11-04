package neoread

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Straight quotes need pairing; unambiguous closing quotes also work in an
// excerpt whose opening quote was on an earlier page. Apostrophes stay in words.
func sentenceClosingQuotes(text string) map[int]bool {
	closing := map[int]bool{}
	double, single, slashes := false, false, 0
	var previous rune
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	for at, r := range text {
		if r == '\\' {
			slashes++
			previous = r
			continue
		}
		escaped := slashes%2 == 1
		slashes = 0
		_, size := utf8.DecodeRuneInString(text[at:])
		next, _ := utf8.DecodeRuneInString(text[at+size:])
		apostrophe := (r == '\'' || r == '’') && word(previous) && (word(next) || r == '\'' && !single)
		if !escaped && !apostrophe {
			switch r {
			case '“':
				double = true
			case '”':
				closing[at], double = true, false
			case '‘':
				single = true
			case '’':
				closing[at], single = true, false
			case '"':
				closing[at] = double
				double = !double
			case '\'':
				closing[at] = single
				single = !single
			default:
				if strings.ContainsRune("»›」』", r) {
					closing[at] = true
				}
			}
		}
		previous = r
	}
	return closing
}

// Retain the original text and offsets, moving only closing-quote prefixes
// across a platform sentence boundary (PDF extraction can insert a space).
func reattachSentenceQuotes(parts []string) []string {
	text := strings.Join(parts, "")
	closing := sentenceClosingQuotes(text)
	if len(closing) == 0 || len(parts) < 2 {
		return parts
	}
	result := make([]string, len(parts))
	start, boundary := 0, 0
	for i, part := range parts {
		boundary += len(part)
		end := boundary
		if end < start {
			end = start
		}
		for end < len(text) {
			at := end
			for at < len(text) {
				r, size := utf8.DecodeRuneInString(text[at:])
				if !unicode.IsSpace(r) {
					break
				}
				at += size
			}
			if !closing[at] {
				break
			}
			_, size := utf8.DecodeRuneInString(text[at:])
			end = at + size
		}
		result[i], start = text[start:end], end
	}
	return result
}

func reattachTranslationQuotes(pairs []TranslationSentence) []TranslationSentence {
	source, target, anchors := make([]string, len(pairs)), make([]string, len(pairs)), make([]string, len(pairs))
	for i, pair := range pairs {
		source[i], target[i], anchors[i] = pair.Source, pair.Target, pair.Anchor
		if anchors[i] == "" {
			anchors[i] = pair.Source
		}
	}
	fixedSource, fixedTarget, fixedAnchors := reattachSentenceQuotes(source), reattachSentenceQuotes(target), reattachSentenceQuotes(anchors)
	changed := false
	for i := range pairs {
		changed = changed || fixedSource[i] != source[i] || fixedTarget[i] != target[i] || fixedAnchors[i] != anchors[i]
	}
	if !changed {
		return pairs
	}
	source, target, anchors = fixedSource, fixedTarget, fixedAnchors
	// A quote-only pair can disappear from both sides. Keep ambiguous mappings
	// intact rather than dropping actual translated prose on only one side.
	for i := range pairs {
		if (strings.TrimSpace(source[i]) == "") != (strings.TrimSpace(target[i]) == "") {
			return pairs
		}
	}
	result := make([]TranslationSentence, 0, len(pairs))
	for i, pair := range pairs {
		if strings.TrimSpace(source[i]) == "" && strings.TrimSpace(target[i]) == "" {
			continue
		}
		if source[i] != pair.Source {
			pair.Source = strings.TrimSpace(source[i])
		}
		pair.Target = target[i]
		if pair.Anchor != "" || anchors[i] != pair.Source {
			pair.Anchor = anchors[i]
		}
		result = append(result, pair)
	}
	return result
}
