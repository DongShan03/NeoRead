package neoread

import (
	"regexp"
	"strings"
)

var marginPageNumber = regexp.MustCompile(`(?i)^(?:page\s+)?[-–—]?\s*(?:\d{1,5}|[ivxlcdm]{1,8})\s*[-–—]?$`)
var marginArxiv = regexp.MustCompile(`(?i)^arxiv\s*:\s*\d{4}\.\d+(?:v\d+)?(?:\s*\[[a-z.]+\])?(?:\s+\d{1,2}\s+[a-z]{3,9}\s+\d{4})?$`)

// Keep in sync with neoread-core/pdf-content.ts. Semantic labels take precedence.
func isPDFPageDecoration(b PDFBlock) bool {
	if (b.Bounds.X < .09 || b.Bounds.X > .9) && (b.Bounds.Width < .065 || b.Bounds.Height > .15) && marginArxiv.MatchString(strings.TrimSpace(b.Text)) {
		return true
	}
	switch b.Label {
	case "header", "footer", "number", "header_image", "footer_image":
		return true
	case "", "text":
		return (b.Bounds.Y < .1 || b.Bounds.Y+b.Bounds.Height > .9) && b.Bounds.Width < .18 && b.Bounds.Height < .045 && marginPageNumber.MatchString(strings.TrimSpace(b.Text))
	default:
		return false
	}
}
