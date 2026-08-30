package interp

import (
	"regexp"
	"strconv"
	"strings"
)

// Section is one numbered heading block parsed out of a designer/planning
// document, e.g. "## Sprint 1" or "## Task 3 (depends_on: 1,2)".
type Section struct {
	Label     string
	Index     int
	DependsOn []int
	Body      string
}

// Declarations holds the scalars and numbered sections parsed out of a plan
// document. This generalizes dev.go's parseLimits/countSprints regex scans
// into one reusable parser: any "key: N" line becomes a Scalar, any
// "## <Label> <N>" heading becomes a Section running to the next heading of
// the same label (or end of document).
type Declarations struct {
	Scalars  map[string]int
	Sections []Section
}

var (
	scalarLineRE  = regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*:[ \t]*(\d+)[ \t]*$`)
	sectionHeadRE = regexp.MustCompile(`(?m)^##[ \t]+([A-Za-z]+)[ \t]+(\d+)(?:[ \t]*\(depends_on:[ \t]*([\d,\s]*)\))?.*$`)
)

// ParseDeclarations scans doc for scalar declarations and numbered sections.
func ParseDeclarations(doc string) Declarations {
	scalars := make(map[string]int)
	for _, m := range scalarLineRE.FindAllStringSubmatch(doc, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			scalars[strings.ToLower(m[1])] = n
		}
	}

	locs := sectionHeadRE.FindAllStringSubmatchIndex(doc, -1)
	sections := make([]Section, 0, len(locs))
	for i, loc := range locs {
		label := doc[loc[2]:loc[3]]
		index, _ := strconv.Atoi(doc[loc[4]:loc[5]])
		var deps []int
		if loc[6] != -1 && loc[6] != loc[7] {
			for d := range strings.SplitSeq(doc[loc[6]:loc[7]], ",") {
				d = strings.TrimSpace(d)
				if d == "" {
					continue
				}
				if n, err := strconv.Atoi(d); err == nil {
					deps = append(deps, n)
				}
			}
		}
		end := len(doc)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		sections = append(sections, Section{
			Label:     label,
			Index:     index,
			DependsOn: deps,
			Body:      strings.TrimSpace(doc[loc[0]:end]),
		})
	}
	return Declarations{Scalars: scalars, Sections: sections}
}

// SectionsFor returns every Section whose Label case-insensitively matches
// label, in document order.
func (d Declarations) SectionsFor(label string) []Section {
	var out []Section
	for _, s := range d.Sections {
		if strings.EqualFold(s.Label, label) {
			out = append(out, s)
		}
	}
	return out
}
