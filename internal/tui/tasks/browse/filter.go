package browse

import (
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/sahilm/fuzzy"

	"github.com/kute-dev/kute/internal/resources"
)

// filterMatch is one row that survived the filter, with its fuzzy match
// indexes (into the row's Name) for highlight rendering.
type filterMatch struct {
	row     resources.Row
	matches []int
}

// applyFilter fuzzy-matches query against each row's Name (sahilm/fuzzy,
// the same library and behavior as the jump palette) as a predicate only:
// survivors keep rows' own order, not fuzzy score order. rows arrive
// already sorted (unhealthy-first, a 1-9 column sort, namespace runs), and
// buildDisplayRows' grouping and fold rely on that — a score-ranked list
// split a namespace into two runs and could fold an unhealthy row into a
// "healthy" summary. An empty query matches every row with no highlight
// indexes. best is the index into the result of the highest-scoring match
// (-1 when nothing matched or the query is empty): the order stays the
// list's, but the cursor still belongs on the row the query names best, so
// typing "api" lands on api-… rather than a failing attempts-fail-… that
// sorts above it on a looser a…p…i match.
func applyFilter(rows []resources.Row, query string) (out []filterMatch, best int) {
	if query == "" {
		out = make([]filterMatch, len(rows))
		for i, r := range rows {
			out[i] = filterMatch{row: r}
		}
		return out, -1
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.Name
	}
	found := fuzzy.Find(query, names)
	best = -1
	bestRow := -1
	if len(found) > 0 {
		bestRow = found[0].Index // fuzzy.Find ranks by score, best first
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Index < found[j].Index })
	out = make([]filterMatch, 0, len(found))
	for _, m := range found {
		if m.Index == bestRow {
			best = len(out)
		}
		out = append(out, filterMatch{row: rows[m.Index], matches: m.MatchedIndexes})
	}
	return out, best
}

// highlightName renders name with matched rune positions styled matchStyle
// and the rest styled base — the table-row equivalent of the jump
// palette's match highlighting.
func highlightName(name string, matches []int, matchStyle, base lipgloss.Style) string {
	if len(matches) == 0 {
		return base.Render(name)
	}
	set := make(map[int]bool, len(matches))
	for _, i := range matches {
		set[i] = true
	}
	var b strings.Builder
	for i, r := range []rune(name) {
		if set[i] {
			b.WriteString(matchStyle.Render(string(r)))
		} else {
			b.WriteString(base.Render(string(r)))
		}
	}
	return b.String()
}
