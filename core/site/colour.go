package site

import (
	"fmt"
	"math"
	"strings"
)

// Page backgrounds of the stylesheet; link colours are derived to stay readable on them.
const (
	lightBG = "#ffffff"
	darkBG  = "#101216"
	// minLinkContrast is the WCAG AA ratio for normal text.
	minLinkContrast = 4.5
)

func parseHex(h string) (r, g, b uint8, ok bool) {
	_, err := fmt.Sscanf(strings.ToLower(h), "#%02x%02x%02x", &r, &g, &b)
	return r, g, b, err == nil
}

func luminance(h string) float64 {
	r, g, b, ok := parseHex(h)
	if !ok {
		return 0
	}
	lin := func(c uint8) float64 {
		v := float64(c) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrastRatio(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// contrastText picks black or white text for a solid background colour (#rrggbb).
func contrastText(h string) string {
	if luminance(h) > 0.179 {
		return "#000000"
	}
	return "#ffffff"
}

// readable moves colour h towards black (on a light background) or white (on a dark one) in 5%
// steps until it reaches minLinkContrast against bg. Colours that already pass are unchanged.
func readable(h, bg string) string {
	towards := "#000000"
	if luminance(bg) < 0.5 {
		towards = "#ffffff"
	}
	r, g, b, ok := parseHex(h)
	tr, tg, tb, _ := parseHex(towards)
	if !ok {
		return towards
	}
	cur := strings.ToLower(h)
	for i := 1; i <= 20 && contrastRatio(cur, bg) < minLinkContrast; i++ {
		f := float64(i) * 0.05
		mix := func(a, t uint8) uint8 { return uint8(math.Round(float64(a)*(1-f) + float64(t)*f)) }
		cur = fmt.Sprintf("#%02x%02x%02x", mix(r, tr), mix(g, tg), mix(b, tb))
	}
	return cur
}
