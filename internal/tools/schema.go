package tools

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
)

// schemaOf builds the JSON Schema for a tool's input from its input struct,
// so the field a tool decodes and the property a model sees are one
// declaration. Tags: json (the property name), desc (its description),
// req:"true" (required), min and max (array bounds). Strings, integers,
// booleans, floats, slices and nested structs are supported; that is every
// shape a built-in tool uses.
func schemaOf(v any) json.RawMessage {
	b, err := json.Marshal(objectSchema(reflect.TypeOf(v)))
	if err != nil {
		panic(err) // a tool's own struct: a programming error, not an input
	}
	return b
}

func objectSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		p := typeSchema(f.Type)
		if d := f.Tag.Get("desc"); d != "" {
			p["description"] = d
		}
		if m := f.Tag.Get("min"); m != "" {
			p["minItems"], _ = strconv.Atoi(m)
		}
		if m := f.Tag.Get("max"); m != "" {
			p["maxItems"], _ = strconv.Atoi(m)
		}
		props[name] = p
		if f.Tag.Get("req") == "true" {
			required = append(required, name)
		}
	}
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func typeSchema(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem())}
	case reflect.Struct:
		return objectSchema(t)
	case reflect.Pointer:
		return typeSchema(t.Elem())
	case reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.UnsafePointer:
	}
	panic("tools: no JSON schema for " + t.String())
}
