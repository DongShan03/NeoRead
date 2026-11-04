package neoread

import (
	"strings"
	"testing"
)

func TestTranslationPromptAsksForKaTeXInlineMath(t *testing.T) {
	prompt := translationPrompt(Document{Title: "t"}, layoutManifest{Pages: 1}, translationBatch{}, "")
	for _, rule := range []string{"KaTeX", "$T_i$", `\\ll`, `\\$`, "不要输出 $$", "sourceIndex"} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("prompt lacks %q", rule)
		}
	}
}

func TestTranslationKeepsEscapedKaTeXInTarget(t *testing.T) {
	line := `{"sentences":[{"source":"Let T be the followers of t in MVSG.","target":"令 $T_i$ 为 $t_i$ 在 $MVSG(s, \\ll)$ 中的后继，费用为 \\$5。"}]}`
	sentences, err := parseTranslation(line, "Let T be the followers of t in MVSG.")
	if err != nil {
		t.Fatal(err)
	}
	if want := `令 $T_i$ 为 $t_i$ 在 $MVSG(s, \ll)$ 中的后继，费用为 \$5。`; sentences[0].Target != want {
		t.Fatalf("target %q", sentences[0].Target)
	}
}

func TestTranslationAllowsRepairedCentroidSuperscriptsButRejectsAddedVectorLayers(t *testing.T) {
	for _, pair := range [][2]string{
		{`For each layer ℓ, we compute μ ( jail $ℓ)=\mathbb{E}_{x∈\mathcal{D}_{\mathrm{jail}}}[\mathbf{h}^{(ℓ)}(x)]$.`, `对于每层 $\ell$，质心为 $\mu_{\mathrm{jail}}^{(\ell)}=\mathbb{E}_{x\in\mathcal{D}_{\mathrm{jail}}}[\mathbf{h}^{(\ell)}(x)]$。`},
		{`where μ ( jail $ℓ)∈\mathbb{R}^{d}$ and μ ( ref $ℓ)∈\mathbb{R}^{d}$ denote average representations.`, `其中，$\mu_{\mathrm{jail}}^{(\ell)}\in\mathbb{R}^{d}$ 和 $\mu_{\mathrm{ref}}^{(\ell)}\in\mathbb{R}^{d}$ 为平均表征。`},
		{`The vector is $\mathbf{v}=\bar{\mathbf{h}}^{(l)}$.`, `向量为 $\mathbf{v}=\bar{\mathbf{h}^{(l)}}$。`},
	} {
		if !matchingMathDecorations(pair[0], pair[1]) {
			t.Fatal("valid repair or grouping was rejected", pair)
		}
	}
	if matchingMathDecorations(`The vector is $\mathbf{v}=\bar{\mathbf{h}}^{(l)}$.`, `向量为 $\mathbf{v}^{(l)}=\bar{\mathbf{h}}^{(l)}$。`) {
		t.Fatal("an extra layer was added to v")
	}
}

func TestTranslationUsesSentenceIndicesAndRejectsIncompleteOrConflictingIndices(t *testing.T) {
	source := "First sentence. Second sentence."
	good := `{"sentences":[{"sourceIndex":1,"target":"第二句。"},{"sourceIndex":0,"target":"第一句。另一句中文解释。"}]}`
	parts, err := parseTranslation(good, source)
	if err != nil || len(parts) != 2 || parts[0].Source != "First sentence." || parts[1].Target != "第二句。" {
		t.Fatal(parts, err)
	}
	for _, raw := range []string{
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"}]}`,
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"},{"sourceIndex":0,"target":"重复。"}]}`,
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"},{"sourceIndex":2,"target":"越界。"}]}`,
		`{"sentences":[{"sourceIndex":0,"target":"第一句。"},{"source":"Second sentence.","target":"缺编号。"}]}`,
		`{"sentences":[{"sourceIndex":0,"source":"Wrong source.","target":"第一句。"},{"sourceIndex":1,"target":"第二句。"}]}`,
	} {
		if _, err := parseTranslation(raw, source); err == nil {
			t.Fatal("invalid indexed output accepted", raw)
		}
	}
}
