package mysql

import (
	"strings"

	"flexie.io/sag/internal/sqlguard"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"
)

// rewrite is the second half: what the statement asked for that has to be
// written differently for it to be allowed to run at all. A * has to become the
// columns it stands for, once one of them is hidden; a read of the database's
// catalog has to be narrowed to the tables this tool may see, because otherwise
// it answers with the names of the ones it may not.
func (a *analysis) rewrite() {
	// Innermost query first: a * over a subquery can only be expanded once the
	// subquery's own * has been.
	for i := len(a.scopes) - 1; i >= 0; i-- {
		a.expandStars(a.scopes[i])
		if a.reason != "" {
			return
		}
	}
	for _, s := range a.scopes {
		a.narrowCatalog(s)
		if a.reason != "" {
			return
		}
	}
}

// expandStars turns * into the columns it stands for, but only where that
// changes something. A query over tables with nothing hidden in them keeps the *
// it was written with, and the statement is sent on untouched.
func (a *analysis) expandStars(s *scope) {
	if s.sel == nil || s.sel.Fields == nil {
		return
	}
	var out []*ast.SelectField
	expanded := false
	for _, field := range s.sel.Fields.Fields {
		if field.WildCard == nil {
			out = append(out, field)
			continue
		}
		fields, needed, ok := a.expandStar(s, field)
		if !ok {
			return
		}
		if !needed {
			out = append(out, field)
			continue
		}
		out = append(out, fields...)
		expanded = true
	}
	if expanded {
		s.sel.Fields.Fields = out
		a.changed = true
	}
}

func (a *analysis) expandStar(s *scope, field *ast.SelectField) (fields []*ast.SelectField, needed, ok bool) {
	if schema := field.WildCard.Schema.O; schema != "" && !strings.EqualFold(schema, a.guard.Catalog().Database()) {
		a.refuse("You can reach one database, %q, and nothing outside it.", a.guard.Catalog().Database())
		return nil, false, false
	}

	covered := s.order
	if qualifier := strings.ToLower(field.WildCard.Table.O); qualifier != "" {
		src, found := s.sources[qualifier]
		if !found {
			a.refuse("What %s.* stands for could not be established. Name the columns you want.", field.WildCard.Table.O)
			return nil, false, false
		}
		covered = []*source{src}
	}

	for _, src := range covered {
		if src.table == "" {
			continue
		}
		if columns, known := a.guard.Catalog().Columns(src.table); known && a.guard.MasksAnyColumn(src.table, columns) {
			needed = true
			break
		}
	}
	if !needed {
		return nil, false, true
	}
	// A star in a write is the rows going IN. Replacing a hidden column there
	// would not hide anything: it would store the stand-in over the real value,
	// permanently, and the control would be the thing doing the damage.
	if a.write {
		a.refuse("This statement writes rows read with *, and one of those columns is hidden. " +
			"Name the columns you mean to write, leaving the hidden one out.")
		return nil, false, false
	}

	for _, src := range covered {
		columns, known := a.columnsOf(src)
		if !known {
			a.refuse("Which columns * stands for could not be established, and one of the tables has a hidden field. Name the columns you want.")
			return nil, false, false
		}
		for _, column := range columns {
			fields = append(fields, a.selectField(src, column))
		}
	}
	return fields, true, true
}

// columnsOf is what a source contributes to a *: a table's own columns, or the
// names a subquery projects, which may themselves have come from a * that was
// expanded a moment ago.
func (a *analysis) columnsOf(src *source) ([]string, bool) {
	if src.table != "" {
		return a.guard.Catalog().Columns(src.table)
	}
	if src.derivedQuery != nil {
		return a.projectedNames(src.derivedQuery)
	}
	// A name a WITH introduced, or the catalog: neither can be expanded from what
	// is in front of us here.
	return nil, false
}

// projectedNames is what a subquery calls the columns it returns, by the rules
// the database itself uses: an alias if it was given one, the column's own name
// if it is a plain column, and otherwise the text of the expression.
func (a *analysis) projectedNames(n ast.Node) ([]string, bool) {
	switch v := n.(type) {
	case *ast.SelectStmt:
		if v.Fields == nil {
			return nil, false
		}
		var out []string
		for _, field := range v.Fields.Fields {
			if field.WildCard != nil {
				inner, ok := a.starNames(v, field)
				if !ok {
					return nil, false
				}
				out = append(out, inner...)
				continue
			}
			if field.AsName.O != "" {
				out = append(out, field.AsName.O)
				continue
			}
			if column, isColumn := field.Expr.(*ast.ColumnNameExpr); isColumn && column.Name != nil {
				out = append(out, column.Name.Name.O)
				continue
			}
			text, err := restore(field.Expr)
			if err != nil {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	case *ast.SetOprStmt:
		// A set of queries takes its column names from the first of them.
		if v.SelectList == nil || len(v.SelectList.Selects) == 0 {
			return nil, false
		}
		return a.projectedNames(v.SelectList.Selects[0])
	}
	return nil, false
}

// starNames answers what a * that was left alone stands for, so a query built on
// top of one can still be expanded.
func (a *analysis) starNames(sel *ast.SelectStmt, field *ast.SelectField) ([]string, bool) {
	s, known := a.byNode[sel]
	if !known {
		return nil, false
	}
	covered := s.order
	if qualifier := strings.ToLower(field.WildCard.Table.O); qualifier != "" {
		src, found := s.sources[qualifier]
		if !found {
			return nil, false
		}
		covered = []*source{src}
	}
	var out []string
	for _, src := range covered {
		columns, ok := a.columnsOf(src)
		if !ok {
			return nil, false
		}
		out = append(out, columns...)
	}
	return out, true
}

// selectField writes one column of an expanded *: the column itself, or the
// value that stands for it when it is hidden, under the name it would have had.
func (a *analysis) selectField(src *source, column string) *ast.SelectField {
	if src.table != "" && a.guard.MasksColumn(src.table, column) {
		return &ast.SelectField{
			Expr:   ast.NewValueExpr(sqlguard.Hidden, "", ""),
			AsName: ast.NewCIStr(column),
		}
	}
	name := &ast.ColumnName{Name: ast.NewCIStr(column)}
	if !strings.HasPrefix(src.alias, "#") && src.alias != "" {
		name.Table = ast.NewCIStr(src.alias)
	}
	return &ast.SelectField{Expr: &ast.ColumnNameExpr{Name: name}}
}

// narrowCatalog adds to a read of the database's own catalog what the read did
// not ask for: this database only, and only the tables this tool may see. It is
// the half of hiding a table that matters most, because a tool that answers
// "which tables are there" with the name of a hidden one has already told the
// assistant what to go looking for.
func (a *analysis) narrowCatalog(s *scope) {
	if s.sel == nil {
		return
	}
	for _, src := range s.order {
		if src.catalog == "" {
			continue
		}
		qualifier := src.alias
		if strings.HasPrefix(qualifier, "#") {
			qualifier = src.catalog
		}
		a.and(s.sel, equals(qualifier, "table_schema", a.guard.Catalog().Database()))
		a.and(s.sel, a.nameFilter(qualifier, "table_name"))
		if src.catalog == "key_column_usage" {
			// A foreign key names the table it points at, which is a way to learn
			// that a hidden table exists without ever selecting from it.
			a.and(s.sel, orNull(qualifier, "referenced_table_name", a.nameFilter(qualifier, "referenced_table_name")))
		}
	}
}

// and joins a condition onto a query's own, wrapping the query's own first where
// that matters. AND binds tighter than OR, so narrowing "a OR b" by writing
// "a OR b AND mine" would leave the a half of it unnarrowed: exactly the tables
// the policy hides, returned by a query that read as though it had been checked.
func (a *analysis) and(sel *ast.SelectStmt, pred ast.ExprNode) {
	if pred == nil {
		return
	}
	if sel.Where == nil {
		sel.Where = pred
	} else {
		left := sel.Where
		if bindsLooserThanAnd(left) {
			left = &ast.ParenthesesExpr{Expr: left}
		}
		sel.Where = &ast.BinaryOperationExpr{Op: opcode.LogicAnd, L: left, R: pred}
	}
	a.changed = true
}

// bindsLooserThanAnd reports whether an expression would be taken apart by an
// AND written after it.
func bindsLooserThanAnd(expr ast.ExprNode) bool {
	binary, ok := expr.(*ast.BinaryOperationExpr)
	if !ok {
		return false
	}
	return binary.Op == opcode.LogicOr || binary.Op == opcode.LogicXor
}

// nameFilter is the list of tables a catalog read may answer with, written the
// way the policy itself reads: an allowlist asks for the names it permits, so a
// table made after this snapshot was taken is not among them; a denylist asks
// for everything except the names it refuses, so one is.
func (a *analysis) nameFilter(qualifier, column string) ast.ExprNode {
	if a.guard.Policy().TableMode == sqlguard.ModeAllowlist {
		visible := a.guard.VisibleTables()
		if len(visible) == 0 {
			return never()
		}
		return in(qualifier, column, visible, false)
	}
	hidden := a.guard.HiddenTables()
	if len(hidden) == 0 {
		return nil
	}
	return in(qualifier, column, hidden, true)
}

func in(qualifier, column string, names []string, not bool) ast.ExprNode {
	list := make([]ast.ExprNode, 0, len(names))
	for _, name := range names {
		list = append(list, ast.NewValueExpr(name, "", ""))
	}
	return &ast.PatternInExpr{Expr: columnExpr(qualifier, column), List: list, Not: not}
}

func orNull(qualifier, column string, pred ast.ExprNode) ast.ExprNode {
	if pred == nil {
		return nil
	}
	return &ast.ParenthesesExpr{Expr: &ast.BinaryOperationExpr{
		Op: opcode.LogicOr,
		L:  &ast.IsNullExpr{Expr: columnExpr(qualifier, column)},
		R:  pred,
	}}
}

func equals(qualifier, column, value string) ast.ExprNode {
	return &ast.BinaryOperationExpr{
		Op: opcode.EQ,
		L:  columnExpr(qualifier, column),
		R:  ast.NewValueExpr(value, "", ""),
	}
}

// never is a condition nothing satisfies, for an allowlist that permits no table
// the database actually has: the read runs and answers with nothing, rather than
// being written as a list of no names, which is not a thing SQL can say.
func never() ast.ExprNode {
	return &ast.BinaryOperationExpr{Op: opcode.EQ, L: ast.NewValueExpr(0, "", ""), R: ast.NewValueExpr(1, "", "")}
}

func columnExpr(qualifier, column string) *ast.ColumnNameExpr {
	name := &ast.ColumnName{Name: ast.NewCIStr(column)}
	if qualifier != "" {
		name.Table = ast.NewCIStr(qualifier)
	}
	return &ast.ColumnNameExpr{Name: name}
}
