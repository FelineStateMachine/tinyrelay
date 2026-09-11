package seedmark

import "strconv"

func num(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

func circle(x, y, radius float64, fill string) string {
	return `<circle cx="` + num(x) + `" cy="` + num(y) + `" r="` + num(radius) + `" fill="` + fill + `"/>`
}

func ellipse(x, y, rx, ry float64, fill string) string {
	return `<ellipse cx="` + num(x) + `" cy="` + num(y) + `" rx="` + num(rx) + `" ry="` + num(ry) + `" fill="` + fill + `"/>`
}

func rect(x, y, width, height, rx float64, fill string) string {
	return `<rect x="` + num(x) + `" y="` + num(y) + `" width="` + num(width) + `" height="` + num(height) + `" rx="` + num(rx) + `" fill="` + fill + `"/>`
}

func path(d, fill string) string { return strokePath(d, fill, "none", 0) }

func strokePath(d, fill, stroke string, width float64) string {
	return `<path d="` + d + `" fill="` + fill + `" stroke="` + stroke + `" stroke-width="` + num(width) + `" stroke-linecap="round" stroke-linejoin="round"/>`
}

func group(body, transform string) string {
	return `<g transform="` + transform + `">` + body + `</g>`
}
