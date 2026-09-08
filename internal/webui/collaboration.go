package webui

import "encoding/json"

// The in-process backend can return typed slices inside a map. Normalize the
// slice before handing it to templates, just as a JSON client would receive it.
func collaborationSlice(value any, field string) []any {
	items := valueMap(value)[field]
	if rows, ok := items.([]any); ok {
		return rows
	}
	var rows []any
	encoded, err := json.Marshal(items)
	if err == nil {
		_ = json.Unmarshal(encoded, &rows)
	}
	return rows
}

func collaborationItems(value any) []any   { return collaborationSlice(value, "items") }
func collaborationReplies(value any) []any { return collaborationSlice(value, "replies") }

func collaborationLabels(value any) []string {
	var labels []string
	encoded, err := json.Marshal(valueMap(value)["labels"])
	if err == nil {
		_ = json.Unmarshal(encoded, &labels)
	}
	return labels
}
