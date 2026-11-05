package neoread

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

var errTranslationMath = errors.New("译文改变了公式符号、重音、字体或上下标，请重试此段")

var greekMathNames = map[rune]string{
	// PDF fonts can map Greek letters to these Unicode compatibility glyphs.
	'\u2206': "Delta", '\u00b5': "mu", '\u2126': "Omega",
	'α': "alpha", 'β': "beta", 'γ': "gamma", 'δ': "delta", 'ε': "varepsilon", 'ϵ': "epsilon", 'ζ': "zeta", 'η': "eta", 'θ': "theta", 'ϑ': "vartheta", 'ι': "iota", 'κ': "kappa", 'ϰ': "varkappa", 'λ': "lambda", 'μ': "mu", 'ν': "nu", 'ξ': "xi", 'π': "pi", 'ϖ': "varpi", 'ρ': "rho", 'ϱ': "varrho", 'σ': "sigma", 'ς': "varsigma", 'τ': "tau", 'υ': "upsilon", 'φ': "varphi", 'ϕ': "phi", 'χ': "chi", 'ψ': "psi", 'ω': "omega",
	'Γ': "Gamma", 'Δ': "Delta", 'Θ': "Theta", 'Λ': "Lambda", 'Ξ': "Xi", 'Π': "Pi", 'Σ': "Sigma", 'Υ': "Upsilon", 'Φ': "Phi", 'Ψ': "Psi", 'Ω': "Omega",
}

func mathGreekName(name string) bool {
	for _, value := range greekMathNames {
		if name == value {
			return true
		}
	}
	return false
}

// Canonicalize notation for comparison only. Never replace the PDF anchor text.
func canonicalMathSymbols(text string) string {
	greek := []rune("ΑΒΓΔΕΖΗΘΙΚΛΜΝΞΟΠΡϴΣΤΥΦΧΨΩ∇αβγδεζηθικλμνξοπρςστυφχψω∂ϵϑϰϕϱϖ")
	script := []rune("𝒜ℬ𝒞𝒟ℰℱ𝒢ℋℐ𝒥𝒦ℒℳ𝒩𝒪𝒫𝒬ℛ𝒮𝒯𝒰𝒱𝒲𝒳𝒴𝒵")
	blackboard := []rune("𝔸𝔹ℂ𝔻𝔼𝔽𝔾ℍ𝕀𝕁𝕂𝕃𝕄ℕ𝕆ℙℚℝ𝕊𝕋𝕌𝕍𝕎𝕏𝕐ℤ")
	var out strings.Builder
	for _, r := range text {
		atom := ""
		for i, v := range script {
			if r == v {
				atom = `\mathcal{` + string(rune('A'+i)) + `}`
				break
			}
		}
		for i, v := range blackboard {
			if r == v {
				atom = `\mathbb{` + string(rune('A'+i)) + `}`
				break
			}
		}
		if atom != "" {
			out.WriteString(atom)
			continue
		}
		bold := false
		for _, base := range []rune{0x1d6a8, 0x1d6e2, 0x1d71c, 0x1d756, 0x1d790} {
			if r >= base && r < base+rune(len(greek)) {
				r = greek[r-base]
				bold = base != 0x1d6e2
				break
			}
		}
		if name := greekMathNames[r]; name != "" {
			atom = `\` + name + ` `
		} else {
			atom = string(r)
		}
		if bold {
			atom = `\mathbf{` + strings.TrimSpace(atom) + `}`
		}
		out.WriteString(atom)
	}
	return out.String()
}

type inlineMathSpan struct {
	Start, End int
	Formula    string
}

// Original delimiters and mathematical Unicode glyphs identify replaceable
// spans. Ordinary words are never sent back from OCR as replacement prose.
func inlineMathSpans(text string) []inlineMathSpan {
	spans := []inlineMathSpan{}
	for at := 0; at < len(text); {
		start := at
		r, size := utf8.DecodeRuneInString(text[at:])
		at += size
		if r == '\\' {
			if at < len(text) {
				_, n := utf8.DecodeRuneInString(text[at:])
				at += n
			}
			continue
		}
		if r == '$' {
			delimiter := "$"
			if at < len(text) && text[at] == '$' {
				delimiter = "$$"
				at++
			}
			from := at
			for at < len(text) {
				if text[at] == '\\' {
					at += 2
					continue
				}
				if strings.HasPrefix(text[at:], delimiter) {
					spans = append(spans, inlineMathSpan{start, at + len(delimiter), text[from:at]})
					at += len(delimiter)
					break
				}
				_, n := utf8.DecodeRuneInString(text[at:])
				at += n
			}
			continue
		}
		isGlyph := func(r rune) bool {
			return r >= 0x1d400 && r <= 0x1d7ff || greekMathNames[r] != "" || strings.ContainsRune("ℬℰℱℋℐℒℳℛℂℍℕℙℚℝℤ", r)
		}
		if isGlyph(r) {
			for at < len(text) {
				next, n := utf8.DecodeRuneInString(text[at:])
				if isGlyph(next) {
					at += n
					continue
				}
				if next == '_' || next == '^' {
					if _, end, ok := mathArgument(text, at+1); ok {
						at = end
						continue
					}
				}
				break
			}
			spans = append(spans, inlineMathSpan{start, at, canonicalMathSymbols(text[start:at])})
		}
	}
	return spans
}

var inlineFormulaResponse = regexp.MustCompile(`(?s)^\$([^$]+)\$$`)

func inlineFormulaMarkdown(response string) (string, error) {
	text := strings.TrimSpace(response)
	// Image providers sometimes follow the display formula convention. The
	// destination determines the delimiter; an inline recovery stays inline.
	if completeFormula.MatchString(text) && strings.Count(text, "$$") == 2 {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "$$"), "$$"))
	} else if inlineFormulaResponse.MatchString(text) {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	if text == "" || strings.ContainsAny(text, "$`\r\n") || strings.Contains(text, "~~~") {
		return "", errors.New("行内公式识别结果不完整，请重试此段")
	}
	return "$" + text + "$", nil
}

// JSON's valid \t, \b, \f, \n and \r escapes can silently eat the beginning
// of \tau, \bar or \frac. Restore known control words only inside math.
var decodedMathEscape = regexp.MustCompile("[\b\f\n\r\t][A-Za-z]+")
var escapedMathCommands = map[string]bool{
	"tau": true, "theta": true, "tilde": true, "times": true, "text": true, "textbf": true, "textit": true, "textrm": true, "textnormal": true, "top": true, "to": true,
	"bar": true, "beta": true, "boldsymbol": true, "bm": true, "begin": true, "big": true, "bigg": true, "bigsqcup": true, "bigcup": true, "bigcap": true, "binom": true,
	"frac": true, "forall": true,
	"nu": true, "nabla": true, "neq": true, "not": true, "neg": true, "notin": true,
	"rho": true, "rightarrow": true, "right": true, "rangle": true, "rfloor": true, "rceil": true, "rm": true,
}

func repairMathJSONEscapes(text string) string {
	return decodedMathEscape.ReplaceAllStringFunc(text, func(value string) string {
		prefix := map[byte]string{'\b': "b", '\f': "f", '\n': "n", '\r': "r", '\t': "t"}[value[0]]
		name := prefix + value[1:]
		if escapedMathCommands[name] {
			return `\` + name
		}
		return value
	})
}
func repairTranslatedMath(text string) string {
	spans := inlineMathSpans(text)
	var out strings.Builder
	cursor := 0
	for _, span := range spans {
		if text[span.Start] != '$' {
			continue
		}
		out.WriteString(text[cursor:span.Start])
		out.WriteByte('$')
		out.WriteString(repairMathJSONEscapes(span.Formula))
		out.WriteByte('$')
		cursor = span.End
	}
	if cursor == 0 {
		return text
	}
	out.WriteString(text[cursor:])
	return out.String()
}
