package neoread

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/clipperhouse/uax29/v2/sentences"
)

var sentenceAbbreviation = regexp.MustCompile(`(?i)(?:\bet al\.|\be\.g\.|\bi\.e\.|\b(?:fig|eq|sec|vol|no|vs|app)\.)$`)
var mathDecoration = regexp.MustCompile(`\\(bar|overline|hat|widehat|tilde|widetilde|dot|ddot|mathcal|mathbb|mathbf|bigsqcup|sqcup|sum|dagger|ddagger)`)

func matchingMathDecorations(source, target string) bool {
	counts := func(text string) map[string]int {
		result := map[string]int{}
		text = strings.NewReplacer("⨆", `\sqcup`, "⊔", `\sqcup`, "∑", `\sum`, "†", `\dagger`, "‡", `\ddagger`).Replace(text)
		for _, match := range mathDecoration.FindAllStringSubmatchIndex(text, -1) {
			// TeX control words end before _, ^ or braces, but not before another letter.
			if match[3] < len(text) && (text[match[3]] >= 'a' && text[match[3]] <= 'z' || text[match[3]] >= 'A' && text[match[3]] <= 'Z') {
				continue
			}
			name := text[match[2]:match[3]]
			switch name {
			case "bigsqcup":
				name = "sqcup"
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
	// PDF extraction can leave a centroid's layer label outside its math span.
	// Check the superscripts of recognized decorated atoms, rather than the
	// total number of carets in the sentence (which also counts repaired labels).
	before, after := decoratedMathScripts(source), decoratedMathScripts(target)
	for atom, scripts := range before {
		slices.Sort(scripts)
		slices.Sort(after[atom])
		if !slices.Equal(scripts, after[atom]) {
			return false
		}
	}
	return true
}

func mathArgument(text string, at int) (string, int, bool) {
	for at < len(text) && unicode.IsSpace(rune(text[at])) {
		at++
	}
	if at >= len(text) {
		return "", at, false
	}
	if text[at] != '{' {
		start := at
		if text[at] == '\\' {
			at++
			for at < len(text) && (text[at] >= 'a' && text[at] <= 'z' || text[at] >= 'A' && text[at] <= 'Z') {
				at++
			}
			if at == start+1 && at < len(text) {
				at++
			}
		} else {
			_, size := utf8.DecodeRuneInString(text[at:])
			at += size
		}
		return text[start:at], at, true
	}
	start, depth := at+1, 1
	for i := start; i < len(text); i++ {
		if text[i] == '\\' {
			i++
			continue
		}
		if text[i] == '{' {
			depth++
		}
		if text[i] == '}' {
			depth--
			if depth == 0 {
				return text[start:i], i + 1, true
			}
		}
	}
	return "", at, false
}

var mathControlWord = regexp.MustCompile(`\\[A-Za-z]+`)

func decoratedMathScripts(text string) map[string][]int {
	result := map[string][]int{}
	for _, match := range mathDecoration.FindAllStringSubmatchIndex(text, -1) {
		at := match[3]
		if at < len(text) && (text[at] >= 'a' && text[at] <= 'z' || text[at] >= 'A' && text[at] <= 'Z') {
			continue
		}
		name := text[match[2]:match[3]]
		switch name {
		case "overline":
			name = "bar"
		case "widehat":
			name = "hat"
		case "widetilde":
			name = "tilde"
		case "bigsqcup":
			name = "sqcup"
		}
		base, count := "", 0
		if name != "sum" && name != "sqcup" && name != "dagger" && name != "ddagger" {
			argument, end, ok := mathArgument(text, at)
			if !ok {
				continue
			}
			at = end
			count = strings.Count(argument, "^")
			var stripped strings.Builder
			for i := 0; i < len(argument); {
				if argument[i] == '^' || argument[i] == '_' {
					_, end, ok := mathArgument(argument, i+1)
					if !ok {
						break
					}
					i = end
					continue
				}
				stripped.WriteByte(argument[i])
				i++
			}
			for _, r := range mathControlWord.ReplaceAllString(stripped.String(), "") {
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					base += string(r)
				}
			}
			// Compound expressions need richer structure than this atom check.
			if utf8.RuneCountInString(base) != 1 {
				continue
			}
		}
		for at < len(text) && (text[at] == '}' || text[at] == ' ') {
			at++
		}
		for at < len(text) && (text[at] == '^' || text[at] == '_') {
			marker := text[at]
			_, end, ok := mathArgument(text, at+1)
			if !ok {
				break
			}
			if marker == '^' {
				count++
			}
			at = end
			for at < len(text) && text[at] == ' ' {
				at++
			}
		}
		key := name + ":" + base
		result[key] = append(result[key], count)
	}
	return result
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
