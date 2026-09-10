package webui

import (
	"fmt"
	"strconv"
)

func fileSize(value any) string {
	size, err := strconv.ParseFloat(plainString(value), 64)
	if err != nil || size < 0 {
		return ""
	}
	if size < 1024 {
		return fmt.Sprintf("%.0f B", size)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	for i, unit := range units {
		size /= 1024
		if size < 1024 || i == len(units)-1 {
			return fmt.Sprintf("%.1f %s", size, unit)
		}
	}
	return ""
}
