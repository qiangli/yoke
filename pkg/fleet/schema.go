package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type schemaField struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

func nounType(noun string) reflect.Type {
	switch noun {
	case KindTool:
		return reflect.TypeOf(Tool{})
	case KindModel:
		return reflect.TypeOf(Model{})
	case KindAgent:
		return reflect.TypeOf(Agent{})
	case KindCommand:
		return reflect.TypeOf(Command{})
	default:
		return nil
	}
}

func schemaFields(noun string) []schemaField {
	var fields []schemaField
	walkSchema(nounType(noun), "", &fields)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Path < fields[j].Path })
	return fields
}

func walkSchema(t reflect.Type, prefix string, out *[]schemaField) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := yamlName(f)
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		*out = append(*out, schemaField{Path: path, Type: schemaType(f.Type), Description: f.Tag.Get("doc")})

		child := indirectType(f.Type)
		switch child.Kind() {
		case reflect.Struct:
			walkSchema(child, path, out)
		case reflect.Slice:
			elem := indirectType(child.Elem())
			*out = append(*out, schemaField{Path: path + ".<index>", Type: schemaType(child.Elem()), Description: "list element"})
			if elem.Kind() == reflect.Struct {
				walkSchema(elem, path+".<index>", out)
				if _, ok := namedIdentityField(elem); ok {
					*out = append(*out, schemaField{Path: path + ".name=<value>", Type: schemaType(child.Elem()), Description: "named list element"})
					walkSchema(elem, path+".name=<value>", out)
				}
			}
		case reflect.Map:
			elem := indirectType(child.Elem())
			*out = append(*out, schemaField{Path: path + ".<key>", Type: schemaType(child.Elem()), Description: "map value"})
			if elem.Kind() == reflect.Struct {
				walkSchema(elem, path+".<key>", out)
			}
		}
	}
}

func indirectType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func yamlName(f reflect.StructField) string {
	name := strings.Split(f.Tag.Get("yaml"), ",")[0]
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name
}

func schemaType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "uint"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.String:
		return "string"
	case reflect.Slice:
		return "[]" + schemaType(t.Elem())
	case reflect.Map:
		return "map[" + schemaType(t.Key()) + "]" + schemaType(t.Elem())
	case reflect.Struct:
		return "object"
	default:
		return t.String()
	}
}

func writeSchema(w io.Writer, noun string, asJSON bool) error {
	fields := schemaFields(noun)
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(fields)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tTYPE\tMEANING")
	for _, f := range fields {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Path, f.Type, f.Description)
	}
	return tw.Flush()
}

func newSchema(noun string) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:           "schema",
		Short:         "List fields accepted by --set, --unset, and --field",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeSchema(cmd.OutOrStdout(), noun, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}
