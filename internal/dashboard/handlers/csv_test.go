package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// R10-28 / R11-26: a waitlist email such as =HYPERLINK("http://evil","x")@x.y
// is a formula the moment an admin opens the export in a spreadsheet.
func TestCSVSafe_PrefixesFormulaCells(t *testing.T) {
	cases := map[string]string{
		`=HYPERLINK("http://evil.test","x")@r12.test`: `'=HYPERLINK("http://evil.test","x")@r12.test`,
		"+1-555@x.test":     "'+1-555@x.test",
		"-cmd|' /C calc":    "'-cmd|' /C calc",
		"@SUM(1)":           "'@SUM(1)",
		"\t=1+1":            "'\t=1+1",
		"alice@example.com": "alice@example.com",
		"":                  "",
	}
	for in, want := range cases {
		assert.Equal(t, want, csvSafe(in), in)
	}
	assert.Equal(t, []string{"'=x", "landing"}, csvSafeRow("=x", "landing\r"))
}
