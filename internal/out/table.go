package out

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"
)

// writeTable renders data as a human-readable table or key/value listing,
// used when stdout is a TTY (see IsTTY). It mirrors the shape of the JSON a
// non-TTY caller would get, just formatted for a terminal instead of an
// agent: a slice of structs becomes a column table, a single struct or map
// becomes a "field: value" listing, anything else is printed as-is.
func writeTable(w io.Writer, data any, verbose bool) {
	if data == nil {
		fmt.Fprintln(w, "(no data)")
		return
	}

	v := reflect.ValueOf(data)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			fmt.Fprintln(w, "(no data)")
			return
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		writeSlice(w, v, verbose)
	case reflect.Struct:
		writeKV(w, structFields(v, verbose))
	case reflect.Map:
		writeKV(w, mapFields(v))
	default:
		fmt.Fprintln(w, flatten(v))
	}
}

func writeSlice(w io.Writer, v reflect.Value, verbose bool) {
	if v.Len() == 0 {
		fmt.Fprintln(w, "(no results)")
		return
	}

	elem := v.Index(0)
	for elem.Kind() == reflect.Ptr {
		elem = elem.Elem()
	}

	if elem.Kind() != reflect.Struct {
		for i := 0; i < v.Len(); i++ {
			fmt.Fprintln(w, flatten(v.Index(i)))
		}
		return
	}

	headers := fieldNames(elem.Type(), verbose)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(strings.Join(headers, "\t")))
	for i := 0; i < v.Len(); i++ {
		row := v.Index(i)
		for row.Kind() == reflect.Ptr {
			row = row.Elem()
		}
		fmt.Fprintln(tw, strings.Join(rowCells(row, headers, verbose), "\t"))
	}
	tw.Flush()
}

type kv struct {
	name  string
	value string
}

func structFields(v reflect.Value, verbose bool) []kv {
	t := v.Type()
	var out []kv
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, skip := jsonFieldName(f)
		if skip || (isVerboseField(f) && !verbose) {
			continue
		}
		// Anonymous embedded structs with no explicit json tag are promoted
		// by encoding/json, so mirror that here rather than showing one
		// opaque "EmbeddedType: {...}" row.
		if f.Anonymous && f.Tag.Get("json") == "" {
			fv := v.Field(i)
			for fv.Kind() == reflect.Ptr {
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				out = append(out, structFields(fv, verbose)...)
				continue
			}
		}
		out = append(out, kv{name: name, value: flatten(v.Field(i))})
	}
	return out
}

func rowCells(v reflect.Value, headers []string, verbose bool) []string {
	fields := structFields(v, verbose)
	byName := make(map[string]string, len(fields))
	for _, f := range fields {
		byName[f.name] = f.value
	}
	cells := make([]string, len(headers))
	for i, h := range headers {
		cells[i] = byName[h]
	}
	return cells
}

func fieldNames(t reflect.Type, verbose bool) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, skip := jsonFieldName(f)
		if skip || (isVerboseField(f) && !verbose) {
			continue
		}
		if f.Anonymous && f.Tag.Get("json") == "" {
			ft := f.Type
			for ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				names = append(names, fieldNames(ft, verbose)...)
				continue
			}
		}
		names = append(names, name)
	}
	return names
}

// isVerboseField reports whether a struct field carries a `verbose:"…"` tag.
// Such fields are included in table/key-value terminal output only when the
// caller opted into verbose rendering (--verbose); JSON output
// always includes them.
func isVerboseField(f reflect.StructField) bool {
	return f.Tag.Get("verbose") != ""
}

// jsonFieldName mirrors encoding/json's tag rules closely enough for
// display purposes: `json:"-"` and unexported fields are skipped, an
// explicit name is honored, otherwise the Go field name is used.
func jsonFieldName(f reflect.StructField) (name string, skip bool) {
	if f.PkgPath != "" {
		return "", true
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	parts := strings.Split(tag, ",")
	if parts[0] != "" {
		return parts[0], false
	}
	return f.Name, false
}

func mapFields(v reflect.Value) []kv {
	keys := v.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	out := make([]kv, len(keys))
	for i, k := range keys {
		out[i] = kv{name: fmt.Sprint(k), value: flatten(v.MapIndex(k))}
	}
	return out
}

func writeKV(w io.Writer, fields []kv) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, f := range fields {
		fmt.Fprintf(tw, "%s:\t%s\n", f.name, f.value)
	}
	tw.Flush()
}

// flatten renders a single value for a table cell or key/value line:
// scalars print directly, []string joins with ", ", anything more complex
// (nested structs, maps, slices of structs) falls back to compact JSON
// rather than a bespoke recursive layout.
func flatten(v reflect.Value) string {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		if v.IsNil() {
			return ""
		}
	}

	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return fmt.Sprint(v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprint(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprint(v.Uint())
	case reflect.Float32, reflect.Float64:
		return fmt.Sprint(v.Float())
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.String {
			parts := make([]string, v.Len())
			for i := range parts {
				parts[i] = v.Index(i).String()
			}
			return strings.Join(parts, ", ")
		}
		return compactJSON(v)
	default:
		return compactJSON(v)
	}
}

func compactJSON(v reflect.Value) string {
	if !v.CanInterface() {
		return ""
	}
	b, err := json.Marshal(v.Interface())
	if err != nil {
		return fmt.Sprint(v.Interface())
	}
	return string(b)
}
