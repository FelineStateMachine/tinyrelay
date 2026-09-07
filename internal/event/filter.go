package event

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Filter struct {
	IDs     []string            `json:"ids,omitempty"`
	Authors []string            `json:"authors,omitempty"`
	Kinds   []int               `json:"kinds,omitempty"`
	Tags    map[string][]string `json:"-"`
	Since   *int64              `json:"since,omitempty"`
	Until   *int64              `json:"until,omitempty"`
	Limit   *int                `json:"limit,omitempty"`
	Search  string              `json:"search,omitempty"`
}

func ParseFilter(raw []byte) (Filter, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return Filter{}, errorsFilter("filter must be an object")
	}
	f := Filter{Tags: make(map[string][]string)}
	for key, value := range values {
		switch {
		case key == "ids", key == "authors":
			if !isJSONArray(value) {
				return Filter{}, errorsFilter(key + " must be a list of strings")
			}
			var out []string
			if err := json.Unmarshal(value, &out); err != nil {
				return Filter{}, errorsFilter(key + " must be a list of strings")
			}
			if key == "ids" {
				f.IDs = out
			} else {
				f.Authors = out
			}
		case key == "kinds":
			if !isJSONArray(value) {
				return Filter{}, errorsFilter("kinds must be a list of integers")
			}
			if err := json.Unmarshal(value, &f.Kinds); err != nil {
				return Filter{}, errorsFilter("kinds must be a list of integers")
			}
		case key == "since", key == "until":
			var n int64
			if err := json.Unmarshal(value, &n); err != nil {
				return Filter{}, errorsFilter(key + " must be an integer")
			}
			if key == "since" {
				f.Since = &n
			} else {
				f.Until = &n
			}
		case key == "limit":
			var n int
			if err := json.Unmarshal(value, &n); err != nil {
				return Filter{}, errorsFilter("limit must be an integer")
			}
			f.Limit = &n
		case key == "search":
			if err := json.Unmarshal(value, &f.Search); err != nil {
				return Filter{}, errorsFilter("search must be a string")
			}
		case len(key) == 2 && key[0] == '#':
			if !isJSONArray(value) {
				return Filter{}, errorsFilter(key + " must be a list of strings")
			}
			var out []string
			if err := json.Unmarshal(value, &out); err != nil {
				return Filter{}, errorsFilter(key + " must be a list of strings")
			}
			f.Tags[key[1:]] = out
		}
	}
	return f, nil
}

func (f Filter) MarshalJSON() ([]byte, error) {
	values := make(map[string]json.RawMessage, 7+len(f.Tags))
	if f.IDs != nil {
		values["ids"] = mustRaw(f.IDs)
	}
	if f.Authors != nil {
		values["authors"] = mustRaw(f.Authors)
	}
	if f.Kinds != nil {
		values["kinds"] = mustRaw(f.Kinds)
	}
	if f.Since != nil {
		values["since"] = mustRaw(f.Since)
	}
	if f.Until != nil {
		values["until"] = mustRaw(f.Until)
	}
	if f.Limit != nil {
		values["limit"] = mustRaw(f.Limit)
	}
	if f.Search != "" {
		values["search"] = mustRaw(f.Search)
	}
	for key, tags := range f.Tags {
		values["#"+key] = mustRaw(tags)
	}
	return json.Marshal(values)
}

func Matches(f Filter, e Event) bool {
	if f.IDs != nil && !contains(f.IDs, e.ID) || f.Authors != nil && !contains(f.Authors, e.PubKey) || f.Kinds != nil && !containsInt(f.Kinds, e.Kind) {
		return false
	}
	if f.Since != nil && e.CreatedAt < *f.Since || f.Until != nil && e.CreatedAt > *f.Until {
		return false
	}
	for name, values := range f.Tags {
		if !tagMatch(e, name, values) {
			return false
		}
	}
	terms := SearchTerms(f.Search)
	if len(terms) == 0 {
		return true
	}
	if IsPrivate(e.Kind) {
		return false
	}
	content := strings.ToLower(e.Content)
	for _, term := range terms {
		if !strings.Contains(content, strings.ToLower(term)) {
			return false
		}
	}
	return true
}

func SearchTerms(query string) []string { return slicesFilter(strings.Fields(query)) }

func slicesFilter(words []string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		if !strings.Contains(word, ":") {
			out = append(out, word)
		}
	}
	return out
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func tagMatch(e Event, name string, values []string) bool {
	for _, tag := range e.Tags {
		if len(tag) > 1 && tag[0] == name && contains(values, tag[1]) {
			return true
		}
	}
	return false
}
func mustRaw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
func errorsFilter(message string) error { return fmt.Errorf("%s", message) }

func isJSONArray(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return len(trimmed) > 0 && trimmed[0] == '['
}
