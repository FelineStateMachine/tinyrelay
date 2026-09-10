package webui

import "html/template"

func jobsFragment(result any, queryErr error) string {
	if queryErr != nil {
		return `<tbody><tr><td role="status">` + template.HTMLEscapeString(queryErr.Error()) + `</td></tr></tbody>`
	}
	rows := make([][]string, 0)
	for _, row := range browseRows(result) {
		values, _ := row.(map[string]any)
		rows = append(rows, []string{plainString(nestedValue(values, "spec", "id")), plainString(firstValue(values, "phase", "status")), plainString(firstValue(values, "finished", "started"))})
	}
	return tableRows([]string{"Job", "Status", "Updated"}, rows)
}

func nestedValue(values map[string]any, keys ...string) any {
	var current any = values
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[key]
	}
	return current
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return ""
}
