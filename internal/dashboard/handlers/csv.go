package handlers

import "strings"

// csvSafe neutralises spreadsheet formula injection in an exported cell: a
// value that begins with =, +, -, @ or a tab/CR is executed as a formula by
// Excel, LibreOffice and Google Sheets when the CSV is opened (OWASP "CSV
// Injection"). The waitlist and audit exports carry user-supplied text
// (every waitlist email comes from the public form — R10-28 / R11-26), so
// such cells are prefixed with a single quote, which spreadsheets render as
// literal text. encoding/csv still quotes the field as needed.
func csvSafe(cell string) string {
	if cell == "" {
		return cell
	}
	switch cell[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + cell
	}
	return cell
}

// csvSafeRow applies csvSafe to every cell.
func csvSafeRow(cells ...string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = csvSafe(strings.TrimRight(c, "\r"))
	}
	return out
}
