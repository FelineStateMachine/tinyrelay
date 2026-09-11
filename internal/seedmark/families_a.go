package seedmark

import "fmt"

func masks(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	w, h := float64(r.Silhouette.Width), float64(r.Silhouette.Height)
	x, y := 32-w/2, 33-h/2
	var art string
	switch r.Layout.Variant {
	case 0:
		art = path("M17 23L10 9L28 19M47 23L54 9L36 19", cool)
	case 1:
		art = rect(20, 8, 24, 16, 4, hot)
	case 2:
		art = path("M17 23L20 7L32 16L44 7L47 23Z", hot)
	case 3:
		art = circle(32, 13, 10, cool) + circle(32, 13, 4, bg)
	}
	switch r.Silhouette.Variant {
	case 0:
		art += path(fmt.Sprintf("M%s %sQ32 %s %s %sL%s 43L32 56L%s 43Z", num(x), num(y), num(y-6), num(64-x), num(y), num(64-x-2), num(x+2)), light)
	case 1:
		art += ellipse(32, 33, w/2, h/2, light)
	case 2:
		art += rect(x, y, w, h, 8, light)
	case 3:
		art += path(fmt.Sprintf("M32 %sL%s 25L%s 47L32 54L%s 47L%s 25Z", num(y-3), num(64-x), num(64-x-4), num(x+4), num(x)), light)
	}
	if r.Features.Accent != 0 {
		art += path("M32 18L39 25L36 42L32 47Z", hot)
	} else {
		art += path("M32 18L39 25L36 42L32 47Z", cool)
	}
	switch r.Features.Variant {
	case 0:
		art += ellipse(24, 31, 5, 6, bg) + ellipse(40, 31, 5, 6, bg)
	case 1:
		art += path("M18 28L29 32L19 35ZM35 32L46 28L45 35Z", bg)
	case 2:
		art += rect(18, 28, 12, 6, 2, bg) + circle(40, 31, 5, bg)
	case 3:
		art += strokePath("M18 33Q24 23 29 33M35 33Q40 23 46 33", "none", bg, 4)
	}
	if r.Features.Accent != 0 {
		art += rect(26, 43, 12, 4, 2, bg)
	} else {
		art += path("M27 42L32 48L37 42Z", bg)
	}
	return art
}

func creatures(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	w := float64(r.Silhouette.Width)
	var art string
	switch r.Layout.Variant {
	case 0:
		art = path("M22 27Q4 9 14 9Q24 10 27 26M36 26Q45 4 51 13L43 29", hot)
	case 1:
		art = path("M19 29L8 20L13 39M43 28L55 19L51 40", cool)
	case 2:
		art = strokePath("M25 25L23 10M39 25L44 12", "none", light, 5) + circle(23, 10, 5, hot) + circle(44, 12, 5, hot)
	case 3:
		art = path("M17 25L15 13L28 22L36 11L40 23L50 18L47 30Z", hot)
	}
	art += strokePath("M22 43L17 52L25 52M40 43L47 52L38 52", "none", cool, 6)
	switch r.Silhouette.Variant {
	case 0:
		art += ellipse(32, 35, w/2, 17, cool)
	case 1:
		art += path("M15 43Q10 19 32 22Q52 10 50 43Q43 55 32 46Q20 55 15 43Z", cool)
	case 2:
		art += rect(32-w/2, 22, w, 27, 10, cool)
	case 3:
		art += path("M12 42L23 20Q32 12 42 26L53 43Q33 58 12 42Z", cool)
	}
	switch r.Features.Variant {
	case 0:
		art += circle(32, 33, 10, light) + circle(32+float64(r.Features.Accent)*3, 33, 4, bg)
	case 1:
		art += ellipse(24, 32, 6, 8, light) + ellipse(40, 32, 6, 8, light) + circle(25, 33, 3, bg) + circle(41, 33, 3, bg)
	case 2:
		art += rect(19, 27, 26, 11, 5, bg) + circle(25, 32, 3, light) + circle(38, 32, 3, light)
	case 3:
		for i := 0; i < r.Features.Count; i++ {
			px := 20 + float64(i)*24/float64(r.Features.Count-1)
			py := 31 + float64(i%2)*4
			art += circle(px, py, 4, light) + circle(px, py+1, 2, bg)
		}
	}
	if r.Features.Accent != 0 {
		art += strokePath("M28 43Q32 49 37 42", "none", bg, 3)
	} else {
		art += rect(29, 42, 7, 4, 2, bg)
	}
	return art
}

func botanical(r recipe) string {
	light, hot, cool := r.Palette[1], r.Palette[2], r.Palette[3]
	w := float64(r.Silhouette.Width)
	var art string
	switch r.Layout.Variant {
	case 0:
		art = path("M23 43L25 55L40 55L43 43Z", hot)
	case 1:
		art = ellipse(32, 48, 18, 7, hot)
	case 2:
		art = path("M20 52L25 44L39 44L46 52Z", light)
	case 3:
		art = strokePath("M25 54L32 45L39 54M20 50L32 45L45 49", "none", hot, 4)
	}
	art += strokePath("M32 46Q28 30 33 14", "none", light, 4)
	switch r.Silhouette.Variant {
	case 0:
		art += path(fmt.Sprintf("M31 35Q%s 37 12 21Q29 17 31 35ZM32 29Q35 12 51 13Q52 29 32 29Z", num(32-w/2)), cool)
	case 1:
		art += path("M31 41Q11 45 12 34Q15 28 30 34M32 28Q48 33 51 23Q51 14 33 20", cool)
	case 2:
		art += path("M31 39Q8 29 22 13Q35 20 31 39ZM33 37Q55 32 46 19Q32 23 33 37Z", cool)
	case 3:
		art += path("M30 40L13 29L27 27L15 16L31 23L38 9L40 26L53 19L45 36Z", cool)
	}
	bloomX, bloomY := 29.0, 16.0
	if r.Features.Variant%2 != 0 {
		bloomX = 41
	}
	if r.Features.Variant >= 2 {
		bloomY = 24
	}
	switch r.Features.Variant {
	case 0:
		art += circle(bloomX-6, bloomY, 7, hot) + circle(bloomX+6, bloomY, 7, hot) + circle(bloomX, bloomY-6, 7, hot) + circle(bloomX, bloomY+5, 7, hot) + circle(bloomX, bloomY, 4, light)
	case 1:
		art += path("M29 20L30 9L37 14L42 7L47 15L54 11L51 22Q42 32 29 20Z", hot) + circle(42, 21, 4, light)
	case 2:
		for i := 0; i < r.Features.Count; i++ {
			art += circle(19+float64(i)*6, 16+float64(i%2)*6, 4, hot)
		}
	case 3:
		art += ellipse(40, 18, 9, 13, hot) + strokePath("M40 10L40 25", "none", light, 3)
	}
	return art
}

func orbital(r recipe) string {
	bg, light, hot, cool := r.Palette[0], r.Palette[1], r.Palette[2], r.Palette[3]
	w := float64(r.Silhouette.Width)
	radius := 11 + (w-26)/3
	var art string
	switch r.Layout.Variant {
	case 0:
		art = strokePath("M9 39Q1 18 31 12Q58 6 55 30Q49 53 23 52", "none", cool, 4)
	case 1:
		art = strokePath("M12 13L49 50M12 50L49 13", "none", cool, 4)
	case 2:
		art = strokePath("M9 32A23 23 0 1 1 32 55", "none", cool, 5)
	case 3:
		art = rect(12, 12, 40, 40, 12, cool) + rect(17, 17, 30, 30, 8, bg)
	}
	switch r.Silhouette.Variant {
	case 0:
		art += circle(32, 32, radius, hot)
	case 1:
		art += rect(18, 18, 28, 28, 6, hot)
	case 2:
		art += path("M32 14L50 32L32 50L14 32Z", hot)
	case 3:
		art += ellipse(32, 32, radius, 19, hot)
	}
	switch r.Features.Variant {
	case 0:
		art += strokePath("M10 37Q32 46 53 25", "none", light, 5)
	case 1:
		art += circle(29, 29, 7, light) + circle(33, 26, 7, hot)
	case 2:
		art += strokePath("M21 25L43 25M21 34L43 34M27 42L37 42", "none", light, 4)
	case 3:
		art += circle(32, 32, 7, bg) + circle(32, 32, 3, light)
	}
	for i := 0; i < r.Features.Count; i++ {
		px := []float64{13, 49, 16, 46, 32}[i]
		py := []float64{16, 46, 47, 13, 7}[i]
		pr := 4.0
		if i%2 != 0 {
			pr = 3
		}
		art += circle(px, py, pr, light)
	}
	return art
}
