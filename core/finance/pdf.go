package finance

// A PDF rendering of invoices and credit notes (specs/062-invoice-email), written here instead of
// taking a PDF library. It writes PDF 1.4 with the two standard fonts Courier and Courier-Bold,
// which every PDF reader supplies: nothing is embedded, so no font or font metrics are shipped,
// and every glyph is 600/1000 of the font size wide, which makes alignment and wrapping exact.
//
// Limit: the standard fonts are used with WinAnsiEncoding (Windows-1252). Western European text,
// including the euro sign and typographic quotes, is written as it is. Every other character
// (Greek, Cyrillic, Chinese, Arabic, Hebrew, emoji, many Central European letters ...) is written
// as "?", and the page then says so and points at the HTML view, which shows all text.

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/unicode/norm"
)

// A4 in points.
const (
	pageW, pageH = 595.0, 842.0
	pdfMargin    = 50.0
	pdfSize      = 9.0
	pdfLeading   = 12.0
	pdfCharW     = pdfSize * 0.6 // Courier: 600 units per glyph
	pdfCols      = 91            // characters between the margins at pdfSize
)

const lossyNote = "Some characters cannot be shown in this PDF and appear as \"?\". The online view shows all text."

// winAnsi encodes text for the standard fonts. Control characters become spaces, invisible format
// characters are dropped and anything outside Windows-1252 becomes "?" (lossy is then true).
func winAnsi(s string) (out []byte, lossy bool) {
	for _, r := range norm.NFC.String(s) {
		switch {
		case unicode.IsControl(r) || r == 0x2028 || r == 0x2029:
			out = append(out, ' ')
		case unicode.Is(unicode.Cf, r):
		case r < 0x7f:
			out = append(out, byte(r))
		default:
			b, ok := charmap.Windows1252.EncodeRune(r)
			if !ok || b < 0x80 {
				b, lossy = '?', true
			}
			out = append(out, b)
		}
	}
	return out, lossy
}

// pdfString writes a PDF literal string; bytes outside printable ASCII are octal escapes, so the
// file stays plain ASCII.
func pdfString(b []byte) string {
	var w strings.Builder
	w.WriteByte('(')
	for _, c := range b {
		switch {
		case c == '(' || c == ')' || c == '\\':
			w.WriteByte('\\')
			w.WriteByte(c)
		case c < 0x20 || c > 0x7e:
			fmt.Fprintf(&w, "\\%03o", c)
		default:
			w.WriteByte(c)
		}
	}
	w.WriteByte(')')
	return w.String()
}

// pdfDoc lays out text from the top of a page downwards, starting new pages as needed.
type pdfDoc struct {
	pages []*bytes.Buffer
	y     float64
	lossy bool
	// head is drawn at the top of every page after the first.
	head func()
}

func (d *pdfDoc) newPage() {
	d.pages = append(d.pages, &bytes.Buffer{})
	d.y = pageH - pdfMargin
	if len(d.pages) > 1 && d.head != nil {
		d.head()
	}
}

// bottom is where the body ends: below it are the footer lines.
const bottom = pdfMargin + 3*pdfLeading

func (d *pdfDoc) need(lines int) {
	if d.y-float64(lines-1)*pdfLeading < bottom {
		d.newPage()
	}
}

// put writes text at a character column of the current line.
func (d *pdfDoc) put(col int, bold bool, size float64, s string) {
	b, lossy := winAnsi(s)
	d.lossy = d.lossy || lossy
	d.raw(pdfMargin+float64(col)*pdfCharW, d.y, bold, size, b)
}

func (d *pdfDoc) raw(x, y float64, bold bool, size float64, b []byte) {
	font := "/F1"
	if bold {
		font = "/F2"
	}
	fmt.Fprintf(d.pages[len(d.pages)-1], "BT %s %g Tf %.2f %.2f Td %s Tj ET\n", font, size, x, y, pdfString(b))
}

// right writes text so that it ends at the given column.
func (d *pdfDoc) right(end int, bold bool, s string) {
	d.put(max(0, end-textCols(s)), bold, pdfSize, s)
}

func (d *pdfDoc) down(n int) { d.y -= float64(n) * pdfLeading }

func (d *pdfDoc) line(col int, bold bool, s string) {
	d.need(1)
	d.put(col, bold, pdfSize, s)
	d.down(1)
}

// rule draws a thin line across the page just above the current line.
func (d *pdfDoc) rule() {
	fmt.Fprintf(d.pages[len(d.pages)-1], "0.5 w %.2f %.2f m %.2f %.2f l S\n", pdfMargin, d.y+pdfSize, pageW-pdfMargin, d.y+pdfSize)
}

// textCols is how many columns a string takes once encoded.
func textCols(s string) int {
	b, _ := winAnsi(s)
	return len(b)
}

// wrap splits text into lines of at most width columns, at spaces where it can; line breaks in
// the text are kept.
func wrap(s string, width int) []string {
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		cur := ""
		for _, word := range strings.Fields(para) {
			for textCols(word) > width { // a word longer than the line is cut
				if cur != "" {
					out, cur = append(out, cur), ""
				}
				r := []rune(norm.NFC.String(word))
				n := min(width, len(r))
				out = append(out, string(r[:n]))
				word = string(r[n:])
			}
			switch {
			case word == "":
			case cur == "":
				cur = word
			case textCols(cur)+1+textCols(word) <= width:
				cur += " " + word
			default:
				out, cur = append(out, cur), word
			}
		}
		out = append(out, cur)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// bytes assembles the file: catalog, page tree, two fonts, information and one page and content
// stream per page, with the cross-reference table. It holds no creation date, so the same invoice
// gives the same bytes.
func (d *pdfDoc) bytes(title string) []byte {
	for i := range d.pages {
		y := pdfMargin
		if d.lossy {
			d.pages[i].WriteString(textAt(pdfMargin, y+pdfLeading, 7, lossyNote))
		}
		d.pages[i].WriteString(textAt(pdfMargin, y, 7, fmt.Sprintf("Page %d of %d. Generated by can-finance; not tax, accounting or legal advice.", i+1, len(d.pages))))
	}
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	const first = 6 // the first page object; each page takes two numbers
	kids := make([]string, len(d.pages))
	for i := range d.pages {
		kids[i] = fmt.Sprintf("%d 0 R", first+2*i)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(d.pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Courier-Bold /Encoding /WinAnsiEncoding >>")
	t, _ := winAnsi(title)
	obj(fmt.Sprintf("<< /Title %s /Producer (can-finance) >>", pdfString(t)))
	for i, p := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %g %g] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>", pageW, pageH, first+2*i+1))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", p.Len(), p.String()))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 5 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

func textAt(x, y, size float64, s string) string {
	b, _ := winAnsi(s)
	return fmt.Sprintf("BT /F1 %g Tf %.2f %.2f Td %s Tj ET\n", size, x, y, pdfString(b))
}

// Column positions of the line table: the description starts at 0, the numbers end where given.
const (
	descCols  = 34
	qtyEnd    = 44
	unitEnd   = 60
	discEnd   = 67
	taxEnd    = 75
	amountEnd = pdfCols
)

// RenderInvoicePDF renders an invoice or credit note. It shows what the public HTML view shows and
// nothing else.
func RenderInvoicePDF(v *Invoice, seller Seller) []byte {
	kind := "INVOICE"
	if v.Kind == KindCreditNote {
		kind = "CREDIT NOTE"
	}
	title := strings.TrimSpace(kind + " " + deref(v.Number))
	d := &pdfDoc{}
	d.newPage()
	d.put(0, true, 15, title)
	d.down(2)
	switch v.Status {
	case StVoid:
		d.put(0, true, 15, "VOID")
		d.down(2)
	case StDraft:
		d.put(0, true, 15, "DRAFT")
		d.down(2)
	}

	// From and To side by side.
	const half = 47
	block := func(label string, parts ...string) []string {
		out := []string{label}
		for _, p := range parts {
			if p != "" {
				out = append(out, wrap(p, half-3)...)
			}
		}
		return out
	}
	taxID := func(s string) string {
		if s == "" {
			return ""
		}
		return "Tax ID: " + s
	}
	from := block("From", seller.Name, seller.Address, taxID(seller.TaxID))
	to := block("To", "(buyer is frozen when the invoice is approved)")
	if s := v.PartySnapshot; s != nil {
		to = block("To", s.Name, s.Address, taxID(s.TaxID))
	}
	for i := range max(len(from), len(to)) {
		d.need(1)
		if i < len(from) {
			d.put(0, i == 0, pdfSize, from[i])
		}
		if i < len(to) {
			d.put(half, i == 0, pdfSize, to[i])
		}
		d.down(1)
	}
	d.down(1)
	d.line(0, false, "Issued:   "+v.IssueDate)
	d.line(0, false, "Due:      "+v.DueDate)
	d.line(0, false, "Currency: "+v.Currency)
	if v.Currency != v.BaseCurrency {
		d.line(0, false, fmt.Sprintf("Rate:     1 %s = %s %s", v.Currency, v.FXRate, v.BaseCurrency))
	}
	d.down(1)

	header := func() {
		d.put(0, true, pdfSize, "Description")
		d.right(qtyEnd, true, "Qty")
		d.right(unitEnd, true, "Unit price")
		d.right(discEnd, true, "Disc %")
		d.right(taxEnd, true, "Tax %")
		d.right(amountEnd, true, "Amount")
		d.down(1)
		d.rule()
		d.down(1)
	}
	d.need(3)
	header()
	d.head = header
	for _, l := range v.Lines {
		desc := wrap(l.Description, descCols)
		if len(desc) == 0 {
			desc = []string{""}
		}
		nums := []struct {
			end, width int
			label, s   string
		}{
			{qtyEnd, qtyEnd - descCols - 1, "qty", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%03d", l.QuantityMilli/1000, l.QuantityMilli%1000), "0"), ".")},
			{unitEnd, unitEnd - qtyEnd - 1, "unit", FormatMinor(l.UnitPriceMinor, v.Currency)},
			{discEnd, discEnd - unitEnd - 1, "disc %", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%02d", l.DiscountBP/100, l.DiscountBP%100), "0"), ".")},
			{taxEnd, taxEnd - discEnd - 1, "tax %", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%04d", l.TaxRatePPM/10000, l.TaxRatePPM%10000), "0"), ".")},
			{amountEnd, amountEnd - taxEnd - 1, "amount", FormatMinor(l.TotalMinor, v.Currency)},
		}
		fits := true
		for _, n := range nums {
			fits = fits && len(n.s) <= n.width
		}
		// Numbers too wide for their columns go on lines of their own, labelled, so that none is cut
		// or printed over another.
		var spill []string
		if !fits {
			var parts []string
			for _, n := range nums {
				parts = append(parts, n.label+" "+n.s)
			}
			spill = wrap(strings.Join(parts, ", "), pdfCols-2)
		}
		d.need(min(len(desc)+len(spill), 6))
		for i, s := range desc {
			d.need(1)
			d.put(0, false, pdfSize, s)
			if i == 0 && fits {
				for _, n := range nums {
					d.right(n.end, false, n.s)
				}
			}
			d.down(1)
		}
		for _, s := range spill {
			d.line(2, false, s)
		}
	}
	d.head = nil

	type total struct {
		label, amount string
		bold          bool
	}
	cur := v.Currency
	totals := []total{{"Subtotal", FormatMinor(v.SubtotalMinor, cur), false}}
	if v.DiscountMinor != 0 {
		totals = append(totals, total{"Discounts", "-" + FormatMinor(v.DiscountMinor, cur), false})
	}
	totals = append(totals, total{"Tax", FormatMinor(v.TaxMinor, cur), false}, total{"Total " + cur, FormatMinor(v.TotalMinor, cur), true})
	if v.PaidMinor != 0 {
		totals = append(totals, total{"Paid", "-" + FormatMinor(v.PaidMinor, cur), false})
	}
	if v.CreditedMinor != 0 {
		totals = append(totals, total{"Credited", "-" + FormatMinor(v.CreditedMinor, cur), false})
	}
	if v.Kind == KindInvoice {
		totals = append(totals, total{"Balance due " + cur, FormatMinor(v.BalanceMinor, cur), true})
	}
	d.need(len(totals) + 1)
	d.rule()
	for _, t := range totals {
		d.need(1)
		d.right(min(discEnd, amountEnd-textCols(t.amount)-2), t.bold, t.label)
		d.right(amountEnd, t.bold, t.amount)
		d.down(1)
	}
	text := func(heading, s string) {
		if strings.TrimSpace(s) == "" {
			return
		}
		d.down(1)
		d.need(2)
		if heading != "" {
			d.line(0, true, heading)
		}
		for _, l := range wrap(s, pdfCols) {
			d.line(0, false, l)
		}
	}
	text("", v.Notes)
	if v.Kind == KindInvoice {
		text("How to pay", v.PaymentInstructions)
	}
	return d.bytes(title)
}

// pdfFilename is the download name: the document number (a series and digits) or "document".
func pdfFilename(v *Invoice) string {
	n := deref(v.Number)
	if n == "" || strings.ContainsFunc(n, func(r rune) bool {
		return !(r == '-' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z')
	}) {
		n = "document"
	}
	return n + ".pdf"
}
