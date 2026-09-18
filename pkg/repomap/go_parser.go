package repomap

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"unicode"
)

// parseGoFile extracts top-level symbols from a Go source file using go/ast.
func parseGoFile(path, relPath string) []Symbol {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil
	}

	var symbols []Symbol

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := Symbol{
				Name:     d.Name.Name,
				Kind:     "func",
				File:     relPath,
				Line:     fset.Position(d.Pos()).Line,
				Exported: isExported(d.Name.Name),
			}

			if d.Recv != nil && len(d.Recv.List) > 0 {
				sym.Kind = "method"
				sym.Receiver = formatReceiverType(d.Recv.List[0].Type)
			}

			sym.Signature = formatFuncSignature(d)
			symbols = append(symbols, sym)

		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					sym := Symbol{
						Name:     s.Name.Name,
						File:     relPath,
						Line:     fset.Position(s.Pos()).Line,
						Exported: isExported(s.Name.Name),
					}

					switch s.Type.(type) {
					case *ast.InterfaceType:
						sym.Kind = "interface"
						sym.Signature = "type " + s.Name.Name + " interface"
					case *ast.StructType:
						sym.Kind = "type"
						sym.Signature = "type " + s.Name.Name + " struct"
					default:
						sym.Kind = "type"
						sym.Signature = "type " + s.Name.Name
					}

					symbols = append(symbols, sym)

				case *ast.ValueSpec:
					for _, name := range s.Names {
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						symbols = append(symbols, Symbol{
							Name:     name.Name,
							Kind:     kind,
							File:     relPath,
							Line:     fset.Position(name.Pos()).Line,
							Exported: isExported(name.Name),
						})
					}
				}
			}
		}
	}

	return symbols
}

// isExported returns true if the name starts with an uppercase letter.
func isExported(name string) bool {
	if name == "" {
		return false
	}
	return unicode.IsUpper(rune(name[0]))
}

// formatReceiverType formats a method receiver type expression.
func formatReceiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + formatReceiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return formatReceiverType(t.X) + "[...]"
	default:
		return "?"
	}
}

// formatFuncSignature produces a concise function signature.
func formatFuncSignature(d *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")

	if d.Recv != nil && len(d.Recv.List) > 0 {
		b.WriteString("(")
		b.WriteString(formatReceiverType(d.Recv.List[0].Type))
		b.WriteString(") ")
	}

	b.WriteString(d.Name.Name)
	b.WriteString("(")

	// Format parameters — names only, types abbreviated.
	if d.Type.Params != nil {
		var params []string
		for _, p := range d.Type.Params.List {
			typeStr := formatTypeExpr(p.Type)
			if len(p.Names) == 0 {
				params = append(params, typeStr)
			} else {
				for _, name := range p.Names {
					params = append(params, name.Name+" "+typeStr)
				}
			}
		}
		b.WriteString(strings.Join(params, ", "))
	}

	b.WriteString(")")

	// Format return types.
	if d.Type.Results != nil && len(d.Type.Results.List) > 0 {
		b.WriteString(" ")
		if len(d.Type.Results.List) == 1 && len(d.Type.Results.List[0].Names) == 0 {
			b.WriteString(formatTypeExpr(d.Type.Results.List[0].Type))
		} else {
			b.WriteString("(")
			var rets []string
			for _, r := range d.Type.Results.List {
				typeStr := formatTypeExpr(r.Type)
				if len(r.Names) == 0 {
					rets = append(rets, typeStr)
				} else {
					for _, name := range r.Names {
						rets = append(rets, name.Name+" "+typeStr)
					}
				}
			}
			b.WriteString(strings.Join(rets, ", "))
			b.WriteString(")")
		}
	}

	return b.String()
}

// formatTypeExpr produces a concise string for a type expression.
func formatTypeExpr(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + formatTypeExpr(t.X)
	case *ast.SelectorExpr:
		return formatTypeExpr(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + formatTypeExpr(t.Elt)
	case *ast.MapType:
		return "map[" + formatTypeExpr(t.Key) + "]" + formatTypeExpr(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + formatTypeExpr(t.Value)
	case *ast.Ellipsis:
		return "..." + formatTypeExpr(t.Elt)
	default:
		return "?"
	}
}
