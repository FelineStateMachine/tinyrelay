package webui

import "strings"

// customViewState is the row state on the Views page: paused when the
// owner or the relay paused the view, active otherwise.
func customViewState(view any) string {
	if valueMap(view)["enabled"] == false {
		return "paused"
	}
	return "active"
}

// customViewKinds lists the kinds a custom view watches.
func customViewKinds(view any) string {
	return joinValues(valueMap(view)["kinds"])
}

// customViewLanguages lists the fenced block languages a custom view renders.
func customViewLanguages(view any) string {
	return joinValues(valueMap(view)["languages"])
}

func joinValues(value any) string {
	items, _ := value.([]any)
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, plainString(item))
	}
	return strings.Join(parts, ", ")
}
