package local

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// BM25 ranking for tool_search (issue #480): a Go port of pi's
// rankDocuments semantics (packages/durable/src/search.ts) — coverage over
// raw score, name-field boost, exact-name pinning, local-name bonus, body-
// only length normalization (k1 1.2, b 0.75), simple tokenization with
// light stemming and an English stop-word list.
//
// Deviations from pi, deliberate: the Unicode word segmenter is a plain
// space split (tool metadata is overwhelmingly ASCII identifiers), the NFC
// normalization is skipped for the same reason, and the namespace field is
// derived from the `__`-joined name prefix (milk tools carry no separate
// namespace description).

// toolSearchDoc is a tool as the ranker sees it: its name and its body
// text (description + parameter names + descriptions — see toolSearchText).
type toolSearchDoc struct {
	name string
	text string
}

const (
	bm25K1              = 1.2
	bm25B               = 0.75
	bm25NameWeight      = 3.0 // a name match beats any number of text mentions (text is capped at K1+1 = 2.2 per term)
	bm25NamespaceWeight = 0.5
)

var (
	camelSplitRE1   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	camelSplitRE2   = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	nonWordRE       = regexp.MustCompile(`[^\p{L}\p{M}\p{N}]+`)
	simpleWordRE    = regexp.MustCompile(`^[a-z0-9]+$`)
	suffixToolRE    = regexp.MustCompile(`(ches|shes|sses|xes|zes)$`)
	invalidNameRE   = regexp.MustCompile(`[^a-z0-9_$]`)
	toolStopWordSet = map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "as": true,
		"at": true, "be": true, "by": true, "for": true, "from": true,
		"in": true, "is": true, "it": true, "of": true, "on": true,
		"or": true, "that": true, "the": true, "this": true, "to": true,
		"with": true,
	}
)

// rankToolSearchDocs orders documents best-first for the query and returns
// their names, ALL matches (the caller slices to its limit and reports the
// rest as "also matched"). Semantics, matching pi's rankDocuments:
//
//   - a tool matching more distinct query terms ranks above one matching
//     fewer, whatever the raw scores (coverage beats score);
//   - a query term in the NAME scores more than any number of text mentions;
//   - a tool whose whole local name (after the last `__`) is in the query
//     gets a bonus worth one more match of every query term;
//   - a query that is exactly a tool's name (or its name after a `__`
//     suffix) ranks that tool first, before coverage;
//   - only the body text is length-normalized;
//   - ties keep document order.
func rankToolSearchDocs(query string, documents []toolSearchDoc) []string {
	queryTerms := sortedUniqueTokens(tokenizeToolText(query))
	if len(queryTerms) == 0 || len(documents) == 0 {
		return nil
	}
	querySet := map[string]bool{}
	for _, t := range queryTerms {
		querySet[t] = true
	}
	indexed := make([]bm25Doc, 0, len(documents))
	totalLen := 0.0
	for _, d := range documents {
		id := indexToolDoc(d)
		totalLen += float64(id.textLength)
		indexed = append(indexed, id)
	}
	avgLen := totalLen / float64(len(indexed))
	if avgLen == 0 {
		avgLen = 1
	}
	// Document frequency per query term over name, body and namespace.
	idf := make(map[string]float64, len(queryTerms))
	fullNameBonus := 0.0
	for _, term := range queryTerms {
		df := 0
		for _, d := range indexed {
			if d.nameTerms[term] || d.textCounts[term] > 0 || d.namespaceTerms[term] {
				df++
			}
		}
		idf[term] = math.Log(1 + (float64(len(indexed))-float64(df)+0.5)/(float64(df)+0.5))
		fullNameBonus += idf[term]
	}
	exactQuery := normalizeToolName(query)
	type match struct {
		name    string
		score   float64
		covered int
		exact   bool
		order   int
	}
	matches := make([]match, 0, len(indexed))
	for i, d := range indexed {
		norm := bm25K1 * (1 - bm25B + bm25B*float64(d.textLength)/avgLen)
		score := 0.0
		covered := 0
		for _, term := range queryTerms {
			count := d.textCounts[term]
			weight := 0.0
			if d.nameTerms[term] {
				weight += bm25NameWeight
			}
			if d.namespaceTerms[term] {
				weight += bm25NamespaceWeight
			}
			if count > 0 {
				weight += float64(count) * (bm25K1 + 1) / (float64(count) + norm)
			}
			if weight > 0 {
				covered++
			}
			score += idf[term] * weight
		}
		if score == 0 {
			continue
		}
		if len(d.localNameTerms) > 0 {
			all := true
			for _, t := range d.localNameTerms {
				if !querySet[t] {
					all = false
					break
				}
			}
			if all {
				score += fullNameBonus
			}
		}
		matches = append(matches, match{name: d.name, score: score, covered: covered, exact: isExactToolNameQuery(exactQuery, d.name), order: i})
	}
	// Insertion sort (stable): exact desc, coverage desc, score desc, document order.
	less := func(a, b match) bool {
		if a.exact != b.exact {
			return a.exact
		}
		if a.covered != b.covered {
			return a.covered > b.covered
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return a.order < b.order
	}
	for i := 1; i < len(matches); i++ {
		for j := i; j > 0 && less(matches[j], matches[j-1]); j-- {
			matches[j-1], matches[j] = matches[j], matches[j-1]
		}
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.name
	}
	return out
}

// bm25Doc is a document split into the fields the ranker weighs differently.
type bm25Doc struct {
	name           string
	nameTerms      map[string]bool
	localNameTerms []string
	textCounts     map[string]int
	textLength     int
	namespaceTerms map[string]bool
}

func indexToolDoc(d toolSearchDoc) bm25Doc {
	textCounts := map[string]int{}
	for _, term := range tokenizeToolText(d.text) {
		textCounts[term]++
	}
	textLength := 0
	for _, c := range textCounts {
		textLength += c
	}
	segments := strings.Split(d.name, "__")
	nameTerms := map[string]bool{}
	for _, t := range tokenizeToolText(d.name) {
		nameTerms[t] = true
	}
	namespaceTerms := map[string]bool{}
	for _, t := range tokenizeToolText(strings.Join(segments[:len(segments)-1], " ")) {
		namespaceTerms[t] = true
	}
	return bm25Doc{
		name:           d.name,
		nameTerms:      nameTerms,
		localNameTerms: sortedUniqueTokens(tokenizeToolText(segments[len(segments)-1])),
		textCounts:     textCounts,
		textLength:     textLength,
		namespaceTerms: namespaceTerms,
	}
}

// tokenizeToolText is pi's tokenize: camelCase boundaries split, lowercased,
// non-alphanumerics to spaces, English stop words dropped, light singular
// stemming. Plain space splitting — see the file header for the deviation.
func tokenizeToolText(text string) []string {
	normalized := camelSplitRE1.ReplaceAllString(text, "$1 $2")
	normalized = camelSplitRE2.ReplaceAllString(normalized, "$1 $2")
	normalized = strings.ToLower(normalized)
	normalized = nonWordRE.ReplaceAllString(normalized, " ")
	words := strings.Fields(normalized)
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w == "" || toolStopWordSet[w] {
			continue
		}
		if simpleWordRE.MatchString(w) {
			w = stemToolTerm(w)
		}
		out = append(out, w)
	}
	return out
}

// stemToolTerm is pi's naive singular form, so `issues` matches `issue` and
// `searches` matches `search`.
func stemToolTerm(term string) string {
	if n := len([]rune(term)); n > 4 && strings.HasSuffix(term, "ies") {
		return term[:len(term)-3] + "y"
	} else if n > 4 && suffixToolRE.MatchString(term) {
		return term[:len(term)-2]
	} else if n > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") {
		return term[:len(term)-1]
	}
	return term
}

// normalizeToolName lowercases with characters invalid in script identifiers
// replaced by `_`, so `dev-radius` matches `dev_radius`.
func normalizeToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return invalidNameRE.ReplaceAllString(name, "_")
}

// isExactToolNameQuery reports whether the whole query is the tool's name:
// its full name or a suffix after a `__` separator, so `thread_read` and
// `app__thread_read` both name `mcp__app__thread_read`.
func isExactToolNameQuery(normalizedQuery, name string) bool {
	normalized := normalizeToolName(name)
	return normalized == normalizedQuery || strings.HasSuffix(normalized, "__"+normalizedQuery)
}

func sortedUniqueTokens(tokens []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
