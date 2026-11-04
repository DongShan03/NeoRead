package neoread

import "testing"

func TestPageDecorationTranslation(t *testing.T) {
	for _, tc := range []struct {
		label, text string
		y, width    float64
		excluded    bool
	}{
		{"header", "Journal", .05, .8, true},
		{"footer_image", "Publisher", .93, .4, true},
		{"number", "12", .8, .04, true},
		{"text", "– 12 –", .95, .04, true},
		{"text", "iv", .95, .04, true},
		{"text", "2026", .4, .04, false},
		{"footnote", "1 Important footnote.", .95, .7, false},
		{"figure_title", "Figure 1. Results", .95, .7, false},
	} {
		t.Run(tc.label+tc.text, func(t *testing.T) {
			b := PDFBlock{Label: tc.label, Text: tc.text}
			b.Bounds.Y, b.Bounds.Width, b.Bounds.Height = tc.y, tc.width, .02
			if got := translationSource(b); (got == "") != tc.excluded {
				t.Fatalf("translationSource=%q, excluded=%v", got, tc.excluded)
			}
		})
	}
}

func TestArxivMarginIsExcludedButBodyReferencesAreRetained(t *testing.T) {
	for _, tc := range []struct {
		text             string
		x, width, height float64
		excluded         bool
	}{
		{"arXiv:2605.18915v1 [cs.CR]", .025, .02, .3, true},
		{"arXiv:2605.18915v1 [cs.CR] 12 May 2026", .95, .02, .3, true},
		{"arXiv:2605.18915v1 [cs.CR]", .15, .6, .02, false},
		{"See arXiv:2605.18915v1 [cs.CR] for details.", .025, .02, .3, false},
	} {
		b := PDFBlock{Label: "text", Text: tc.text}
		b.Bounds.X, b.Bounds.Y, b.Bounds.Width, b.Bounds.Height = tc.x, .2, tc.width, tc.height
		if got := isPDFPageDecoration(b); got != tc.excluded {
			t.Fatalf("margin=%v want=%v: %+v", got, tc.excluded, b)
		}
	}
}
