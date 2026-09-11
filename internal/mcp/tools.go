package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool is one entry in a server's tool table.
type Tool struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Handler      Handler        `json:"-"`
}

// Handler executes a tool. A returned error is reported to the client as a
// tool execution error the model can act on; protocol errors are raised by
// the transport before the handler runs.
type Handler func(ctx context.Context, call Call) (Result, error)

// Call carries the validated arguments of a tools/call request together with
// the authenticated actor.
type Call struct {
	Actor     string
	Name      string
	Arguments map[string]any
	Request   *http.Request
}

// String returns a string argument or "".
func (c Call) String(key string) string {
	value, _ := c.Arguments[key].(string)
	return value
}

// Int returns an integer argument or 0.
func (c Call) Int(key string) int {
	value, _ := c.Arguments[key].(float64)
	return int(value)
}

// Content is one unstructured content block of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Result is what a tool returns. StructuredContent is mirrored into a text
// block when Content is empty so every client sees the same data.
type Result struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Value builds a successful result from a JSON value.
func Value(value any) Result {
	return Result{StructuredContent: value}
}

// Failure builds a tool execution error. Structured data, when given, helps
// the client correct the call.
func Failure(message string, structured any) Result {
	return Result{Content: []Content{{Type: "text", Text: message}}, StructuredContent: structured, IsError: true}
}

func (r Result) filled() Result {
	if len(r.Content) == 0 {
		text, err := json.Marshal(r.StructuredContent)
		if err != nil {
			text = []byte("null")
		}
		r.Content = []Content{{Type: "text", Text: string(text)}}
	}
	return r
}

// Registry is an ordered tool table. Tools list in the order they were
// added so clients can cache the list.
type Registry struct {
	tools []Tool
	index map[string]int
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Add registers a tool. Names must be unique and header safe.
func (r *Registry) Add(tool Tool) error {
	if !toolName.MatchString(tool.Name) {
		return fmt.Errorf("mcp: invalid tool name %q", tool.Name)
	}
	if tool.Handler == nil {
		return fmt.Errorf("mcp: tool %q has no handler", tool.Name)
	}
	if tool.InputSchema == nil {
		tool.InputSchema = Object(nil)
	}
	if r.index == nil {
		r.index = map[string]int{}
	}
	if _, exists := r.index[tool.Name]; exists {
		return fmt.Errorf("mcp: duplicate tool %q", tool.Name)
	}
	r.index[tool.Name] = len(r.tools)
	r.tools = append(r.tools, tool)
	return nil
}

// Tools returns the table in registration order.
func (r *Registry) Tools() []Tool {
	if r == nil {
		return nil
	}
	return append([]Tool(nil), r.tools...)
}

// Lookup finds a tool by name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	if r == nil || r.index == nil {
		return Tool{}, false
	}
	i, ok := r.index[name]
	if !ok {
		return Tool{}, false
	}
	return r.tools[i], true
}

// Object builds a closed object schema. Properties are sorted when listed so
// the schema serializes deterministically.
func Object(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// Validate checks a value against the subset of JSON Schema 2020-12 the tool
// tables use: type, properties, required, additionalProperties, enum, not,
// pattern, minimum, maximum, minLength, maxLength, minProperties and items.
func Validate(schema map[string]any, value any) error {
	return validate(schema, value, "")
}

func validate(schema map[string]any, value any, path string) error {
	where := func() string {
		if path == "" {
			return "argument"
		}
		return path
	}
	if typ, ok := schema["type"].(string); ok {
		if err := checkType(typ, value); err != nil {
			return fmt.Errorf("%s %s", where(), err)
		}
	}
	if options := enumOptions(schema["enum"]); len(options) > 0 && !contains(options, scalar(value)) {
		return fmt.Errorf("%s must be one of %s", where(), strings.Join(options, ", "))
	}
	if excluded, ok := schema["not"].(map[string]any); ok {
		if validate(excluded, value, path) == nil {
			return fmt.Errorf("%s matches an excluded value", where())
		}
	}
	if pattern, ok := schema["pattern"].(string); ok {
		text, _ := value.(string)
		if re, err := regexp.Compile(pattern); err == nil && !re.MatchString(text) {
			return fmt.Errorf("%s does not match %s", where(), pattern)
		}
	}
	if text, ok := value.(string); ok {
		if min, ok := number(schema["minLength"]); ok && float64(utf8.RuneCountInString(text)) < min {
			return fmt.Errorf("%s must not be empty", where())
		}
		if max, ok := number(schema["maxLength"]); ok && float64(utf8.RuneCountInString(text)) > max {
			return fmt.Errorf("%s is longer than %d characters", where(), int(max))
		}
	}
	if n, ok := value.(float64); ok {
		if min, ok := number(schema["minimum"]); ok && n < min {
			return fmt.Errorf("%s must be at least %v", where(), min)
		}
		if max, ok := number(schema["maximum"]); ok && n > max {
			return fmt.Errorf("%s must be at most %v", where(), max)
		}
	}
	if object, ok := value.(map[string]any); ok {
		properties, _ := schema["properties"].(map[string]any)
		for _, key := range required(schema) {
			if _, present := object[key]; !present {
				return fmt.Errorf("%s is required", join(path, key))
			}
		}
		if min, ok := number(schema["minProperties"]); ok && float64(len(object)) < min {
			return fmt.Errorf("%s must not be empty", where())
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			property, known := properties[key].(map[string]any)
			if !known {
				if closed, _ := schema["additionalProperties"].(bool); schema["additionalProperties"] != nil && !closed {
					return fmt.Errorf("%s is not a known argument", join(path, key))
				}
				continue
			}
			if object[key] == nil {
				continue
			}
			if err := validate(property, object[key], join(path, key)); err != nil {
				return err
			}
		}
	}
	if list, ok := value.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range list {
				if err := validate(items, item, fmt.Sprintf("%s[%d]", where(), i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkType(typ string, value any) error {
	switch typ {
	case "string":
		if _, ok := value.(string); !ok {
			return errors.New("must be a string")
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || n != math.Trunc(n) {
			return errors.New("must be an integer")
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return errors.New("must be a number")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return errors.New("must be true or false")
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return errors.New("must be an object")
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return errors.New("must be an array")
		}
	}
	return nil
}

func required(schema map[string]any) []string {
	switch list := schema["required"].(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// enumOptions renders enum members as strings so integer and string enums
// compare the same way.
func enumOptions(value any) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []int:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, strconv.Itoa(item))
		}
		return out
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, scalar(item))
		}
		return out
	}
	return nil
}

func scalar(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
