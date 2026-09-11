package mcp

import (
	"strings"
	"testing"
)

func TestSchemaStringLengthCountsUnicodeCharacters(t *testing.T) {
	schema := map[string]any{"type": "string", "minLength": 1, "maxLength": 500}
	if err := Validate(schema, strings.Repeat("猫", 500)); err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, strings.Repeat("猫", 501)); err == nil {
		t.Fatal("501 characters accepted")
	}
}
