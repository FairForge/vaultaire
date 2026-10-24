package api

import (
	"regexp"
	"strings"
)

// exportSectionFields are the JSON field names a section of the GDPR export
// carries, read from its SQL's select lists (the output names: an alias, or
// the column itself), in order, each once. The "what we can produce" page
// lists them (43.4); TestProduceAccountRowsAreTheExportsSections holds the
// two equal.
func exportSectionFields(s exportSection) []string {
	var out []string
	seen := map[string]bool{}
	for _, q := range s.SQL {
		for _, f := range selectListNames(q) {
			if strings.HasPrefix(f, "_") || seen[f] { // _id is the keyset cursor, never emitted
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

var sqlIdent = regexp.MustCompile(`(?i)(?:\bAS\s+)?"?([a-z_][a-z0-9_]*)"?\s*$`)

// selectListNames splits the outer SELECT … FROM list at depth-0 commas and
// names each item by its alias or its last identifier.
func selectListNames(q string) []string {
	up := strings.ToUpper(q)
	start := strings.Index(up, "SELECT")
	if start < 0 {
		return nil
	}
	i := start + len("SELECT")
	depth, from := 0, -1
	for j := i; j < len(q); j++ {
		switch q[j] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && j+6 <= len(up) && up[j:j+6] == " FROM " || depth == 0 && j+6 <= len(up) && (up[j:j+5] == "\nFROM" || up[j:j+5] == "\tFROM") {
			from = j
			break
		}
	}
	if from < 0 {
		return nil
	}
	var names []string
	depth, last := 0, i
	list := q[i:from]
	emit := func(item string) {
		item = strings.TrimSpace(item)
		if m := sqlIdent.FindStringSubmatch(item); m != nil {
			names = append(names, strings.ToLower(m[1]))
		}
	}
	for j := 0; j < len(list); j++ {
		switch list[j] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				emit(list[last-i : j])
				last = i + j + 1
			}
		}
	}
	emit(list[last-i:])
	return names
}
