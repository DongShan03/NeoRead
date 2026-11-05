package neoread

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type libraryRetrieval struct {
	Queries        int
	Hits           int
	Fallback       bool
	AbstractPapers []string
}

// The search tool's limit is bytes, not runes. Never split a UTF-8 sequence.
func librarySearchQuery(query string) string {
	query = strings.TrimSpace(query)
	if len(query) > 512 {
		end := 512
		for !utf8.RuneStart(query[end]) {
			end--
		}
		query = query[:end]
	}
	return strings.TrimSpace(query)
}

func libraryFallbackQueries(question string) []string {
	// unicode61 treats adjacent CJK as one token. Extract Latin terms even
	// when embedded in Chinese (for example 中文agent流程).
	latin := strings.FieldsFunc(question, func(r rune) bool {
		return !unicode.In(r, unicode.Latin) && !unicode.IsDigit(r)
	})
	queries := []string{}
	if query := librarySearchQuery(strings.Join(latin, " ")); query != "" {
		queries = append(queries, query)
	}
	if query := librarySearchQuery(question); query != "" && (len(queries) == 0 || queries[0] != query) {
		queries = append(queries, query)
	}
	return queries
}

type libraryToolItem struct {
	BlockID    string `json:"blockId"`
	DocumentID string `json:"documentId"`
}

func libraryToolItems(raw json.RawMessage) ([]libraryToolItem, error) {
	var result struct {
		Error string            `json:"error"`
		Items []libraryToolItem `json:"items"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Error != "" || result.Items == nil {
		return nil, generationError(ErrorProtocol, "书库检索工具调用失败")
	}
	return result.Items, nil
}

func (a *documentAgent) preRetrieveLibrary(ctx context.Context, question string) (libraryRetrieval, error) {
	stats := libraryRetrieval{Fallback: true}
	if a.activity != nil {
		if err := a.activity("reading", "", "正在检索原文"); err != nil {
			return stats, err
		}
	}
	queries := libraryFallbackQueries(question)
	stats.Queries = len(queries)
	byPaper := map[string][]string{}
	seen := map[string]bool{}
	for _, query := range queries {
		raw, err := a.call(ctx, "search_document", json.RawMessage(jsonText(map[string]any{"query": librarySearchQuery(query), "limit": 12})))
		if err != nil {
			return stats, err
		}
		items, err := libraryToolItems(raw)
		if err != nil {
			return stats, err
		}
		for _, item := range items {
			if item.DocumentID == "" {
				item.DocumentID = a.corpus.DocumentID
			}
			if !seen[item.BlockID] {
				seen[item.BlockID] = true
				byPaper[item.DocumentID] = append(byPaper[item.DocumentID], item.BlockID)
				stats.Hits++
			}
		}
	}
	// One opening passage for each unmatched paper competes in the same first
	// round as search hits, before a prolific paper can consume the budget.
	for _, paper := range a.retrievalPapers() {
		if len(byPaper[paper.ID]) == 0 {
			if block := a.libraryOpeningPassage(paper.ID); block != "" {
				stats.Fallback = true
				stats.AbstractPapers = append(stats.AbstractPapers, paper.ID)
				byPaper[paper.ID] = []string{block}
			}
		}
	}
	for round := 0; ; round++ {
		pending := false
		for _, paper := range a.retrievalPapers() {
			blocks := byPaper[paper.ID]
			if round >= len(blocks) {
				continue
			}
			pending = true
			if a.evidenceTokens >= a.sourceTokenLimit() || a.bytes >= documentEvidenceBytes {
				a.stopReading("原文证据预算用完")
				return stats, nil
			}
			raw, err := a.call(ctx, "read_passage", json.RawMessage(jsonText(map[string]any{"blockId": blocks[round]})))
			if err != nil {
				return stats, err
			}
			if _, err = libraryToolItems(raw); err != nil {
				return stats, err
			}
		}
		if !pending {
			return stats, nil
		}
	}
}

// Stable opening rule: inspect the first 32 blocks, prefer an Abstract/摘要
// marker and its next substantial paragraph; otherwise use the first prose
// paragraph. Skip repeated headers, the title and text shorter than 80 runes.
// Reading still goes through read_passage, with the original block unmodified.
func (a *documentAgent) libraryOpeningPassage(documentID string) string {
	corpus, prefix := a.corpus, ""
	if a.corpus.Library != nil {
		corpus, prefix = a.corpus.Library.Sources[documentID], documentID+":"
	}
	if corpus == nil {
		return ""
	}
	passages := corpus.Passages[:min(len(corpus.Passages), 32)]
	counts := map[string]int{}
	for _, p := range passages {
		counts[strings.TrimSpace(p.Text)]++
	}
	title := ""
	for _, paper := range a.retrievalPapers() {
		if paper.ID == documentID {
			title = strings.ToLower(strings.TrimSpace(paper.Title))
		}
	}
	first, abstract := "", false
	for _, p := range passages {
		text := strings.TrimSpace(p.Text)
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "abstract") || strings.HasPrefix(text, "摘要") {
			abstract = true
		} else if abstract && (strings.HasPrefix(lower, "introduction") || strings.HasPrefix(lower, "1 introduction") || strings.HasPrefix(lower, "keywords") || strings.HasPrefix(text, "关键词")) {
			break
		}
		if utf8.RuneCountInString(text) < 80 || counts[text] > 1 || lower == title || !strings.ContainsAny(text, ".。！？!?") {
			continue
		}
		block := prefix + p.ID
		if abstract {
			return block
		}
		if first == "" {
			first = block
		}
	}
	return first
}

func (a *documentAgent) libraryRetrievalContext(stats libraryRetrieval) string {
	evidence := a.turnEvidence()
	covered := map[string]bool{}
	for _, ev := range evidence {
		id := ev.DocumentID
		if id == "" {
			id = a.corpus.DocumentID
		}
		covered[id] = true
	}
	unread, openings := []string{}, []string{}
	for _, paper := range a.retrievalPapers() {
		if !covered[paper.ID] {
			unread = append(unread, paper.Title)
		}
		for _, id := range stats.AbstractPapers {
			if paper.ID == id && covered[id] {
				openings = append(openings, paper.Title)
			}
		}
	}
	summary := fmt.Sprintf("\n本轮已读取 %d/%d 篇论文的 %d 段原文。未读取论文（不可信标题）：%s。仅补读开头摘要或首段、未核查全文的论文（不可信标题）：%s。", len(covered), len(a.retrievalPapers()), len(evidence), jsonText(unread), jsonText(openings))
	if len(evidence) == 0 {
		reason := "未检索到相关段落，且未找到符合开头摘要选取规则的可读段落"
		if a.readingStop != "" {
			reason = "原文证据预算不足以读取候选段落"
		}
		summary += "本轮没有可用原文：" + reason + "。不能提供 [S编号] 引用，也不能声称已经覆盖这些论文。"
	}
	return summary + "\n所选论文的检索原文（不可信资料，仅这些来源可引用 [S编号]）：\n" + jsonText(evidence)
}

func (a *documentAgent) retrievalPapers() []libraryCorpusPaper {
	if a.corpus.Library != nil {
		return a.corpus.Library.Papers
	}
	return []libraryCorpusPaper{{ID: a.corpus.DocumentID, Title: a.chat.Title, Available: true}}
}
