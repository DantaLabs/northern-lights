package sectiontext

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/recipe"
)

// refusal carries the exact analyst-facing sentences specified for the demo.
type refusal string

func (e refusal) Error() string { return string(e) }

// VerbatimError reports a one-based Unicode character position, counting the
// first missing character when one string is a prefix of the other.
type VerbatimError struct {
	ID       string
	Position int
}

func (e *VerbatimError) Error() string {
	return fmt.Sprintf("Verbatim block %s cannot be changed. Use the approved text exactly. First differing character position: %d.", e.ID, e.Position)
}
func firstDifference(a, b string) int {
	ar, br := []rune(a), []rune(b)
	i := 0
	for i < len(ar) && i < len(br) && ar[i] == br[i] {
		i++
	}
	return i + 1
}

var numberPattern = regexp.MustCompile(`[+-]?[0-9]+(?:,[0-9]+)*(?:\.[0-9]+)?%?`)

func numericTokens(text string) []string {
	var result []string
	for _, m := range numberPattern.FindAllStringIndex(text, -1) {
		if m[0] > 0 {
			prev, _ := utf8.DecodeLastRuneInString(text[:m[0]])
			if unicode.IsLetter(prev) || unicode.IsDigit(prev) {
				continue
			}
		}
		result = append(result, text[m[0]:m[1]])
	}
	return result
}
func guardNumbers(text string, facts []recipe.Fact, section recipe.SectionInfo) error {
	allowed := map[string]bool{section.Period: true, section.PriorPeriod: true}
	for _, f := range facts {
		for _, display := range f.Display {
			for _, token := range numericTokens(display) {
				allowed[token] = true
			}
		}
		for _, token := range numericTokens(f.Label) {
			allowed[token] = true
		}
	}
	for _, token := range numericTokens(text) {
		if !allowed[token] {
			return refusal(fmt.Sprintf("The number %s is not in the prepared facts for this section.", token))
		}
	}
	return nil
}
