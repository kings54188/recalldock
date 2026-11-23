package toolargs

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func Validate(args map[string]any, schema mcp.ToolInputSchema) error {
	if err := validateObject(args, schema.Properties, schema.Required, "", false); err != nil {
		return err
	}
	for key, raw := range args {
		property := schema.Properties[key].(map[string]any)
		if property["type"] == "integer" || property["type"] == "number" {
			args[key], _ = number(raw)
		}
	}
	return nil
}

func validateObject(args map[string]any, properties map[string]any, required []string, prefix string, open bool) error {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		property, known := properties[key]
		if !known {
			if open {
				continue
			}
			allowed := make([]string, 0, len(properties))
			for name := range properties {
				allowed = append(allowed, name)
			}
			sort.Strings(allowed)
			return fmt.Errorf("unknown argument %q; allowed arguments: %s", prefix+key, strings.Join(allowed, ", "))
		}
		if err := validateValue(args[key], property.(map[string]any), prefix+key); err != nil {
			return err
		}
	}
	for _, key := range required {
		if _, ok := args[key]; !ok {
			return fmt.Errorf("argument %q is required", prefix+key)
		}
	}
	return nil
}

func validateValue(value any, property map[string]any, key string) error {
	switch property["type"] {
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("argument %q must be a string", key)
		}
		if err := validateRange(float64(len([]rune(text))), property, "minLength", "maxLength", key); err != nil {
			return err
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean", key)
		}
	case "integer", "number":
		n, ok := number(value)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Errorf("argument %q must be a finite number", key)
		}
		if property["type"] == "integer" {
			bound := math.Min(math.Ldexp(1, strconv.IntSize-1), math.Ldexp(1, 53))
			if math.Trunc(n) != n || n >= bound || n <= -bound {
				return fmt.Errorf("argument %q must be a finite representable integer", key)
			}
		}
		if err := validateRange(n, property, "minimum", "maximum", key); err != nil {
			return err
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("argument %q must be an array", key)
		}
		if err := validateRange(float64(len(items)), property, "minItems", "maxItems", key); err != nil {
			return err
		}
		if itemSchema, ok := property["items"].(map[string]any); ok {
			for i, item := range items {
				if err := validateValue(item, itemSchema, fmt.Sprintf("%s[%d]", key, i)); err != nil {
					return err
				}
			}
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("argument %q must be an object", key)
		}
		properties, _ := property["properties"].(map[string]any)
		closed, _ := property["additionalProperties"].(bool)
		if err := validateObject(object, properties, schemaStrings(property["required"]), key+".", closed || property["additionalProperties"] == nil); err != nil {
			return err
		}
	}
	if enum := schemaStrings(property["enum"]); len(enum) > 0 {
		for _, option := range enum {
			if value == option {
				return nil
			}
		}
		choices := make([]string, len(enum))
		for i, option := range enum {
			choices[i] = strconv.Quote(option)
		}
		return fmt.Errorf("argument %q must be one of: %s", key, strings.Join(choices, ", "))
	}
	return nil
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		number, err := n.Float64()
		return number, err == nil
	}
	return 0, false
}

func schemaStrings(value any) []string {
	if values, ok := value.([]string); ok {
		return values
	}
	var result []string
	if values, ok := value.([]any); ok {
		for _, item := range values {
			result = append(result, item.(string))
		}
	}
	return result
}

func validateRange(value float64, property map[string]any, minKey, maxKey, key string) error {
	if minimum, ok := number(property[minKey]); ok && value < minimum {
		return fmt.Errorf("argument %q violates %s (%g)", key, minKey, minimum)
	}
	if maximum, ok := number(property[maxKey]); ok && value > maximum {
		return fmt.Errorf("argument %q violates %s (%g)", key, maxKey, maximum)
	}
	return nil
}
