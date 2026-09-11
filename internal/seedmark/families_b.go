package seedmark

import "fmt"

func landscapes(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	s, h := r.Silhouette.Variant, r.Silhouette.Height
	f, accent := r.Features.Variant, r.Features.Accent
	horizon := 31 + (float64(h)-28)/2
	art := []string{
		circle(22, 19, 9, hot),
		circle(43, 19, 10, hot),
		rect(14, 12, 34, 9, 4, hot),
		circle(32, 21, 15, hot),
	}[r.Layout.Variant]
	art += []string{
		path("M7 46L21 22L33 37L43 26L57 46V55H7Z", cool),
		path(fmt.Sprintf("M7 %sQ18 21 31 %sQ46 22 57 35V55H7Z", num(horizon), num(horizon)), cool),
		path("M8 53V25H20V38H30V19H40V32H55V53Z", cool),
		path("M7 36L18 28L26 32L39 23L57 33V55H7Z", cool),
	}[s]
	if f == 0 {
		art += path("M31 35L24 45L36 45L28 55H42L45 43L33 43L37 35Z", light)
	}
	if f == 1 {
		art += strokePath("M9 45Q21 40 32 46T55 46M13 53Q23 48 35 53", "none", light, 4)
	}
	if f == 2 {
		art += strokePath("M18 48V35M12 42L18 32L24 42ZM42 50V38M36 43L42 33L48 43Z", light, light, 2)
	}
	if f == 3 {
		art += rect(26, 36, 15, 17, 2, light) + path("M23 36L33 27L44 36Z", light) + rect(31, 43, 5, 10, 0, bg)
	}
	if accent != 0 {
		art += strokePath("M9 18Q13 13 17 18Q21 13 25 18", "none", light, 2)
	} else {
		art += circle(51, 12, 3, light)
	}
	return art
}

func glyphs(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	w := float64(r.Silhouette.Width)
	f, accent := r.Features.Variant, r.Features.Accent
	art := []string{
		circle(32, 32, 24, cool) + circle(32, 32, 18, bg),
		rect(10, 10, 44, 44, 10, cool) + rect(16, 16, 32, 32, 5, bg),
		strokePath("M10 26L26 10M38 10L54 26M54 38L38 54M26 54L10 38", "none", cool, 6),
		strokePath("M10 47V17H24M54 17V47H40", "none", cool, 6),
	}[r.Layout.Variant]
	a, b := 32-w/2+4, 64-(32-w/2+4)
	art += []string{
		strokePath(fmt.Sprintf("M%s 43L32 17L%s 43M24 35H40", num(a), num(b)), "none", light, 7),
		strokePath(fmt.Sprintf("M%s 22H%sL%s 43H%s", num(a), num(b), num(a), num(b)), "none", light, 7),
		strokePath(fmt.Sprintf("M%s 19V44H%sV19M32 20V39", num(a), num(b)), "none", light, 7),
		path("M21 18H37Q49 24 37 32Q50 40 38 46H21Z", light) + rect(27, 24, 9, 5, 2, bg) + rect(27, 36, 10, 5, 2, bg),
	}[r.Silhouette.Variant]
	art += []string{
		circle(32, 32, 5, hot),
		path("M32 24L40 32L32 40L24 32Z", hot),
		rect(26, 27, 12, 10, 2, hot),
		strokePath("M22 37L42 27", "none", hot, 6),
	}[f]
	if accent != 0 {
		art += circle(49, 15, 4, light)
	} else {
		art += circle(15, 49, 4, light)
	}
	return art
}

func woven(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	s, w := r.Silhouette.Variant, float64(r.Silhouette.Width)
	f, count := r.Features.Variant, r.Features.Count
	gap := 10 + (w-26)/6
	art := ""
	if s == 0 {
		for i := 0; i < 3; i++ {
			fill := light
			if i%2 != 0 {
				fill = hot
			}
			art += rect(13+float64(i)*gap, 10, 7, 44, 3, fill)
		}
		for i := 0; i < 3; i++ {
			art += rect(9, 16+float64(i)*gap, 45, 6, 2, cool)
		}
		art += rect(24, 23, 7, 15, 2, hot)
	}
	if s == 1 {
		for i := 0; i < 3; i++ {
			colors := []string{light, hot, cool}
			x := 12 + i*12
			art += strokePath(fmt.Sprintf("M%d 12L%d 25L%d 39L%d 52", x, x+8, x, x+8), "none", colors[i], 8)
		}
	}
	if s == 2 {
		art += strokePath("M12 20Q12 11 22 11H43Q52 11 52 21V43Q52 53 42 53H22Q12 53 12 43Z", "none", light, 6)
		art += strokePath("M21 10V44H43V20H10M54 32H32V54", "none", hot, 7)
		art += rect(28, 22, 8, 8, 1, cool)
	}
	if s == 3 {
		for i := 0; i < 3; i++ {
			colors := []string{light, hot, cool}
			y := 18 + i*13
			art += path(fmt.Sprintf("M10 %dL22 %dL34 %dL46 %dL54 %dL46 %dL34 %dL22 %dZ", y, 10+i*13, y, 10+i*13, y, 26+i*13, y, 26+i*13), colors[i])
		}
	}
	art = group(art, fmt.Sprintf("rotate(%d 32 32) scale(.88) translate(4.36 4.36)", r.Layout.Variant*45))
	art += []string{
		circle(32, 32, 5, bg) + circle(32, 32, 2, light),
		rect(26, 26, 12, 12, 3, hot),
		path("M32 24L40 32L32 40L24 32Z", cool),
		strokePath("M27 28L37 36M37 28L27 36", "none", bg, 4),
	}[f]
	for i := 0; i < count; i++ {
		art += circle(18+float64(i)*7, 56, 2, hot)
	}
	return art
}

func machines(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	s, w := r.Silhouette.Variant, float64(r.Silhouette.Width)
	f, count, accent := r.Features.Variant, r.Features.Count, r.Features.Accent
	x := 32 - w/2
	art := []string{
		strokePath("M22 23L17 11M42 23L49 11", "none", hot, 4) + circle(17, 11, 4, light),
		rect(23, 9, 18, 16, 4, hot) + rect(28, 12, 8, 6, 1, bg),
		strokePath("M12 27L8 39L19 44M51 27L56 39L47 44", "none", hot, 6),
		circle(22, 49, 8, hot) + circle(43, 49, 8, hot),
	}[r.Layout.Variant]
	art += []string{
		rect(x, 20, w, 30, 5, light),
		path(fmt.Sprintf("M%s 19H%sL%s 45L41 52H23L%s 45Z", num(x+5), num(59-x), num(64-x), num(x)), light),
		ellipse(32, 35, w/2, 18, light),
		path("M23 17H41L52 29V44L41 52H23L12 44V29Z", light),
	}[s]
	if f == 0 {
		art += circle(32, 34, 11, bg) + circle(32, 34, 7, cool) + circle(35, 31, 3, light)
	}
	if f == 1 {
		art += rect(20, 26, 24, 17, 3, bg) + strokePath("M24 37L29 30L34 36L40 29", "none", cool, 3)
	}
	if f == 2 {
		art += rect(19, 25, 26, 15, 2, bg)
		for i := 0; i < count; i++ {
			art += rect(22+float64(i)*4, 28, 2, 9, 1, cool)
		}
	}
	if f == 3 {
		art += circle(25, 32, 6, bg) + circle(39, 32, 6, bg) + strokePath("M20 44H44", "none", hot, 4)
	}
	if accent != 0 {
		art += circle(43, 46, 3, hot)
	} else {
		art += circle(21, 46, 3, hot)
	}
	return art
}
