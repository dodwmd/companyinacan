package finance

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// checkPDF verifies the structure a reader relies on (header, trailer, every cross-reference
// offset, every stream length, the page count) and returns the strings drawn, in order, decoded
// back to bytes, and the number of pages.
func checkPDF(t *testing.T, b []byte) (texts []string, pages int) {
	t.Helper()
	s := string(b)
	if !strings.HasPrefix(s, "%PDF-1.4\n") || !strings.HasSuffix(s, "%%EOF\n") {
		t.Fatal("not a PDF")
	}
	m := regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("no startxref")
	}
	at, _ := strconv.Atoi(m[1])
	lines := strings.Split(s[at:], "\n")
	if lines[0] != "xref" {
		t.Fatalf("startxref does not point at the table: %q", lines[0])
	}
	var first, n int
	if _, err := fmt.Sscanf(lines[1], "%d %d", &first, &n); err != nil || first != 0 {
		t.Fatalf("subsection %q", lines[1])
	}
	for i := 1; i < n; i++ {
		off, err := strconv.Atoi(lines[2+i][:10])
		if err != nil || len(lines[2+i]) != 19 || !strings.HasPrefix(s[off:], fmt.Sprintf("%d 0 obj\n", i)) {
			t.Fatalf("object %d: entry %q", i, lines[2+i])
		}
	}
	for _, sm := range regexp.MustCompile(`(?s)<< /Length (\d+) >>\nstream\n(.*?)endstream`).FindAllStringSubmatch(s, -1) {
		if l, _ := strconv.Atoi(sm[1]); l != len(sm[2]) {
			t.Fatalf("stream length %s, is %d", sm[1], len(sm[2]))
		}
	}
	for _, r := range b {
		if r > 0x7e || (r < 0x20 && r != '\n') {
			t.Fatalf("byte %#x in the file", r)
		}
	}
	for _, tm := range regexp.MustCompile(`Td \(((?:\\.|[^\\()])*)\) Tj`).FindAllStringSubmatch(s, -1) {
		var out []byte
		in := tm[1]
		for i := 0; i < len(in); i++ {
			switch {
			case in[i] != '\\':
				out = append(out, in[i])
			case in[i+1] >= '0' && in[i+1] <= '7':
				v, _ := strconv.ParseUint(in[i+1:i+4], 8, 8)
				out = append(out, byte(v))
				i += 3
			default:
				i++
				out = append(out, in[i])
			}
		}
		texts = append(texts, string(out))
	}
	return texts, strings.Count(s, "/Type /Page /Parent")
}

func pdfInvoice() *Invoice {
	num := "INV-00007"
	return &Invoice{Kind: KindInvoice, Status: StSent, Number: &num, Currency: "EUR", BaseCurrency: "USD", FXRate: "1.0850", IssueDate: "2026-11-01", DueDate: "2026-11-15",
		PartySnapshot: &PartySnapshot{Name: "Müller & Söhne (Nürnberg)", Address: "Königstraße 1\n90402 Nürnberg", TaxID: "DE000"},
		Lines: []Line{{Description: `Café “crème” – 5 € \ (a) b)`, QuantityMilli: 1500, UnitPriceMinor: 1000, DiscountBP: 1000, TaxRatePPM: 190000, TotalMinor: 1607}},
		SubtotalMinor: 1500, DiscountMinor: 150, TaxMinor: 257, TotalMinor: 1607, PaidMinor: 600, BalanceMinor: 1007,
		Notes: "Thank you.", PaymentInstructions: "IBAN XX00"}
}

func TestPDFContentAndStructure(t *testing.T) {
	b := RenderInvoicePDF(pdfInvoice(), Seller{Name: "Example Seller", Address: "1 Fake Street", TaxID: "T-1"})
	texts, pages := checkPDF(t, b)
	all := strings.Join(texts, "\n")
	if pages != 1 {
		t.Fatalf("%d pages", pages)
	}
	// Windows-1252: u umlaut is 0xFC, sharp s 0xDF, the euro sign 0x80, quotes 0x93 and 0x94, dash 0x96.
	for _, want := range []string{"INVOICE INV-00007", "M\xfcller & S\xf6hne (N\xfcrnberg)", "K\xf6nigstra\xdfe 1", "Caf\xe9 \x93cr\xe8me\x94 \x96 5 \x80 \\ (a) b)",
		"1.5", "10.00", "19", "16.07", "Subtotal", "-1.50", "Total EUR", "Paid", "-6.00", "Balance due EUR", "10.07", "How to pay", "IBAN XX00", "Rate:     1 EUR = 1.0850 USD"} {
		if !strings.Contains(all, want) {
			t.Errorf("PDF lacks %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, lossyNote) {
		t.Error("nothing was lost, so nothing should say so")
	}
	if !bytes.Equal(b, RenderInvoicePDF(pdfInvoice(), Seller{Name: "Example Seller", Address: "1 Fake Street", TaxID: "T-1"})) {
		t.Error("not deterministic")
	}
	if out := os.Getenv("CAN_PDF_SAMPLE"); out != "" {
		if err := os.WriteFile(out, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPDFSaysWhatItCannotShow(t *testing.T) {
	v := pdfInvoice()
	v.PartySnapshot.Name = "株式会社 Δ"
	v.Lines[0].Description = "a\x00b\tc‮d" // control and bidi characters
	texts, _ := checkPDF(t, RenderInvoicePDF(v, Seller{Name: "Example"}))
	all := strings.Join(texts, "\n")
	for _, want := range []string{"???? ?", lossyNote, "a b cd"} {
		if !strings.Contains(all, want) {
			t.Errorf("PDF lacks %q in\n%s", want, all)
		}
	}
	// A decomposed accent is composed, not lost.
	v = pdfInvoice()
	v.Lines[0].Description = "Café"
	texts, _ = checkPDF(t, RenderInvoicePDF(v, Seller{Name: "Example"}))
	if all := strings.Join(texts, "\n"); !strings.Contains(all, "Caf\xe9") || strings.Contains(all, lossyNote) {
		t.Errorf("%s", all)
	}
}

// Many and long lines and very large numbers: more pages, a repeated table head, nothing cut and
// nothing outside the margins.
func TestPDFLayoutLimits(t *testing.T) {
	v := pdfInvoice()
	v.Status = StVoid
	v.Lines = nil
	for i := range maxLines {
		v.Lines = append(v.Lines, Line{Description: fmt.Sprintf("item-%03d %s %s", i, strings.Repeat("word ", 20), strings.Repeat("x", 80)), QuantityMilli: 1000, UnitPriceMinor: 100, TotalMinor: 100})
	}
	v.Lines[0].UnitPriceMinor, v.Lines[0].TotalMinor, v.Lines[0].QuantityMilli = 900_000_000_000_000_000, 900_000_000_000_000_000, 999_999_999_999
	v.TotalMinor, v.BalanceMinor = 900_000_000_000_000_000, 900_000_000_000_000_000
	v.Notes = strings.Repeat("note ", 900)
	b := RenderInvoicePDF(v, Seller{Name: strings.Repeat("Long Name ", 20)})
	texts, pages := checkPDF(t, b)
	all := strings.Join(texts, "\n")
	heads := strings.Count(all, "Description\n")
	if pages < 5 || heads < 4 || !strings.Contains(all, "VOID") || !strings.Contains(all, "item-199") || strings.Count(all, "9000000000000000.00") < 4 {
		t.Fatalf("%d pages, %d heads", pages, heads)
	}
	if got := strings.Count(all, "x"); got < 80*maxLines {
		t.Fatalf("long words were cut: %d", got)
	}
	for _, m := range regexp.MustCompile(`BT /F\d (\S+) Tf (\S+) (\S+) Td \(((?:\\.|[^\\()])*)\) Tj`).FindAllStringSubmatch(string(b), -1) {
		size, _ := strconv.ParseFloat(m[1], 64)
		x, _ := strconv.ParseFloat(m[2], 64)
		y, _ := strconv.ParseFloat(m[3], 64)
		n := len(regexp.MustCompile(`\\[0-7]{3}|\\.|[^\\]`).FindAllString(m[4], -1))
		if end := x + float64(n)*size*0.6; x < pdfMargin || end > pageW-pdfMargin+0.1 || y < pdfMargin || y > pageH-pdfMargin {
			t.Fatalf("outside the margins at %v,%v to %v: %q", x, y, end, m[4])
		}
	}
}
