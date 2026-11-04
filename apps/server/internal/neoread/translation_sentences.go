package neoread

import (
	"errors"
	"regexp"
	"strings"

	"github.com/clipperhouse/uax29/v2/sentences"
)

var sentenceAbbreviation = regexp.MustCompile(`(?i)(?:\bet al\.|\be\.g\.|\bi\.e\.|\b(?:fig|eq|sec|vol|no)\.)$`)
var mathDecoration = regexp.MustCompile(`\\(bar|overline|hat|widehat|tilde|widetilde|dot|ddot|mathcal|mathbb|mathbf)\b`)

func matchingMathDecorations(source, target string) bool {
	counts := func(text string) map[string]int {
		result := map[string]int{}
		for _, match := range mathDecoration.FindAllStringSubmatch(text, -1) {
			name := match[1]
			switch name {
			case "overline":
				name = "bar"
			case "widehat":
				name = "hat"
			case "widetilde":
				name = "tilde"
			}
			result[name]++
		}
		return result
	}
	expected, actual := counts(source), counts(target)
	for name, count := range expected {
		if actual[name] != count {
			return false
		}
	}
	return len(expected) == 0 || strings.Count(source, "^") == strings.Count(target, "^")
}

// Keep math punctuation and scientific abbreviations inside their sentence.
// The iterator offsets refer to the untouched source, including whitespace.
func translationSentenceParts(text string) []string {
	masked := []byte(text)
	for i := 0; i < len(text); i++ {
		if text[i] != '$' || i > 0 && text[i-1] == '\\' {
			continue
		}
		delimiter := "$"
		if i+1 < len(text) && text[i+1] == '$' {
			delimiter = "$$"
		}
		end := i + len(delimiter)
		for end < len(text) {
			at := strings.Index(text[end:], delimiter)
			if at < 0 {
				end = len(text)
				break
			}
			end += at
			if text[end-1] != '\\' {
				break
			}
			end += len(delimiter)
		}
		if end >= len(text) {
			continue
		}
		for j := i; j < end+len(delimiter); j++ {
			masked[j] = 'x'
		}
		i = end + len(delimiter) - 1
	}
	iterator := sentences.FromString(string(masked))
	parts := []string{}
	pending := ""
	for iterator.Next() {
		pending += text[iterator.Start():iterator.End()]
		if sentenceAbbreviation.MatchString(strings.TrimSpace(pending)) {
			continue
		}
		if strings.TrimSpace(pending) != "" {
			parts = append(parts, pending)
		}
		pending = ""
	}
	if pending != "" {
		parts = append(parts, pending)
	}
	return parts
}

// Split a paragraph-sized legacy pair only when source and target have the
// same ordered sentence boundaries. Otherwise require a sentence-wise reply.
func translationSentencePairs(input []TranslationSentence) ([]TranslationSentence, error) {
	output := []TranslationSentence{}
	for _, sentence := range input {
		source := translationSentenceParts(sentence.Source)
		if len(source) <= 1 {
			output = append(output, sentence)
			continue
		}
		target := translationSentenceParts(sentence.Target)
		if len(target) != len(source) {
			return nil, errors.New("原文须逐句对应译文，请重试此段")
		}
		for i := range source {
			output = append(output, TranslationSentence{Source: strings.TrimSpace(source[i]), Target: target[i]})
		}
	}
	return output, nil
}
