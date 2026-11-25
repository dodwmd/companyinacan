package forms

import (
	"encoding/csv"
	"io"
	"sort"
	"strings"
	"time"
)

// SanitizeCSVCell neutralises spreadsheet formula injection (OWASP "CSV injection"): a
// cell that a spreadsheet could interpret as a formula gets a leading apostrophe.
func SanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// WriteCSV writes leads as CSV: fixed columns, then the sorted union of submitted field
// names. Every cell goes through SanitizeCSVCell.
func WriteCSV(w io.Writer, leads []Lead) error {
	names := map[string]bool{}
	for _, l := range leads {
		for k := range l.Fields {
			names[k] = true
		}
	}
	extra := make([]string, 0, len(names))
	for k := range names {
		extra = append(extra, k)
	}
	sort.Strings(extra)

	cw := csv.NewWriter(w)
	head := append([]string{"id", "form_id", "created_at", "status", "email", "name", "note"}, extra...)
	for i := range head {
		head[i] = SanitizeCSVCell(head[i])
	}
	if err := cw.Write(head); err != nil {
		return err
	}
	for _, l := range leads {
		row := []string{l.ID, l.FormID, l.CreatedAt.UTC().Format(time.RFC3339), l.Status, l.Email, l.Name, l.Note}
		for _, k := range extra {
			row = append(row, l.Fields[k])
		}
		for i := range row {
			row[i] = SanitizeCSVCell(strings.ToValidUTF8(row[i], "?"))
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
