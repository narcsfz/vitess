/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	"errors"
	"fmt"

	"vitess.io/vitess/go/mysql"

	"vitess.io/vitess/go/sqltypes"

	"vitess.io/vitess/go/vt/key"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"

	"vitess.io/vitess/go/vt/vtgate/evalengine"

	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/engine"
)

func buildSelectPlan(query string) func(sqlparser.Statement, ContextVSchema) (engine.Primitive, error) {
	return func(stmt sqlparser.Statement, vschema ContextVSchema) (engine.Primitive, error) {
		sel := stmt.(*sqlparser.Select)

		p, err := handleDualSelects(sel, vschema)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}

		pb := newPrimitiveBuilder(vschema, newJointab(sqlparser.GetBindvars(sel)))
		if err := pb.processSelect(sel, nil, query); err != nil {
			return nil, err
		}
		if err := pb.bldr.Wireup(pb.bldr, pb.jt); err != nil {
			return nil, err
		}
		return pb.bldr.Primitive(), nil
	}
}

// processSelect builds a primitive tree for the given query or subquery.
// The tree built by this function has the following general structure:
//
// The leaf nodes can be a route, vindexFunc or subquery. In the symtab,
// the tables map has columns that point to these leaf nodes. A subquery
// itself contains a builder tree, but it's opaque and is made to look
// like a table for the analysis of the current tree.
//
// The leaf nodes are usually tied together by join nodes. While the join
// nodes are built, they have ON clauses. Those are analyzed and pushed
// down into the leaf nodes as the tree is formed. Join nodes are formed
// during analysis of the FROM clause.
//
// During the WHERE clause analysis, the target leaf node is identified
// for each part, and the PushFilter function is used to push the condition
// down. The same strategy is used for the other clauses.
//
// So, a typical plan would either be a simple leaf node, or may consist
// of leaf nodes tied together by join nodes.
//
// If a query has aggregates that cannot be pushed down, an aggregator
// primitive is built. The current orderedAggregate primitive can only
// be built on top of a route. The orderedAggregate expects the rows
// to be ordered as they are returned. This work is performed by the
// underlying route. This means that a compatible ORDER BY clause
// can also be handled by this combination of primitives. In this case,
// the tree would consist of an orderedAggregate whose input is a route.
//
// If a query has an ORDER BY, but the route is a scatter, then the
// ordering is pushed down into the route itself. This results in a simple
// route primitive.
//
// The LIMIT clause is the last construct of a query. If it cannot be
// pushed into a route, then a primitive is created on top of any
// of the above trees to make it discard unwanted rows.
func (pb *primitiveBuilder) processSelect(sel *sqlparser.Select, outer *symtab, query string) error {
	// Check and error if there is any locking function present in select expression.
	for _, expr := range sel.SelectExprs {
		if aExpr, ok := expr.(*sqlparser.AliasedExpr); ok && sqlparser.IsLockingFunc(aExpr.Expr) {
			return vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "%v allowed only with dual", sqlparser.String(aExpr))
		}
	}
	if sel.SQLCalcFoundRows {
		if outer != nil || query == "" {
			return mysql.NewSQLError(mysql.ERCantUseOptionHere, "42000", "Incorrect usage/placement of 'SQL_CALC_FOUND_ROWS'")
		}
		sel.SQLCalcFoundRows = false
		if sel.Limit != nil {
			builder, err := buildSQLCalcFoundRowsPlan(query, sel, outer, pb.vschema)
			if err != nil {
				return err
			}
			pb.bldr = builder
			return nil
		}
	}

	// Into is not supported in subquery.
	if sel.Into != nil && (outer != nil || query == "") {
		return mysql.NewSQLError(mysql.ERCantUseOptionHere, "42000", "Incorrect usage/placement of 'INTO'")
	}

	if err := pb.processTableExprs(sel.From); err != nil {
		return err
	}

	if rb, ok := pb.bldr.(*route); ok {
		// TODO(sougou): this can probably be improved.
		directives := sqlparser.ExtractCommentDirectives(sel.Comments)
		rb.eroute.QueryTimeout = queryTimeout(directives)
		if rb.eroute.TargetDestination != nil {
			return errors.New("unsupported: SELECT with a target destination")
		}

		if directives.IsSet(sqlparser.DirectiveScatterErrorsAsWarnings) {
			rb.eroute.ScatterErrorsAsWarnings = true
		}
	}

	// Set the outer symtab after processing of FROM clause.
	// This is because correlation is not allowed there.
	pb.st.Outer = outer
	if sel.Where != nil {
		if err := pb.pushFilter(sel.Where.Expr, sqlparser.WhereStr); err != nil {
			return err
		}
	}
	if err := pb.checkAggregates(sel); err != nil {
		return err
	}
	if err := pb.pushSelectExprs(sel); err != nil {
		return err
	}
	if sel.Having != nil {
		if err := pb.pushFilter(sel.Having.Expr, sqlparser.HavingStr); err != nil {
			return err
		}
	}
	if err := pb.pushOrderBy(sel.OrderBy); err != nil {
		return err
	}
	if err := pb.pushLimit(sel.Limit); err != nil {
		return err
	}
	return pb.bldr.PushMisc(sel)
}

func buildSQLCalcFoundRowsPlan(query string, sel *sqlparser.Select, outer *symtab, vschema ContextVSchema) (builder, error) {
	ljt := newJointab(sqlparser.GetBindvars(sel))
	frpb := newPrimitiveBuilder(vschema, ljt)
	err := frpb.processSelect(sel, outer, "")
	if err != nil {
		return nil, err
	}

	statement, err := sqlparser.Parse(query)
	if err != nil {
		return nil, err
	}
	sel2 := statement.(*sqlparser.Select)

	sel2.SQLCalcFoundRows = false
	sel2.OrderBy = nil
	sel2.Limit = nil

	countStartExpr := []sqlparser.SelectExpr{&sqlparser.AliasedExpr{
		Expr: &sqlparser.FuncExpr{
			Name:  sqlparser.NewColIdent("count"),
			Exprs: []sqlparser.SelectExpr{&sqlparser.StarExpr{}},
		},
	}}
	if sel2.GroupBy == nil && sel2.Having == nil {
		// if there is no grouping, we can use the same query and
		// just replace the SELECT sub-clause to have a single count(*)
		sel2.SelectExprs = countStartExpr
	} else {
		// when there is grouping, we have to move the original query into a derived table.
		//                       select id, sum(12) from user group by id =>
		// select count(*) from (select id, sum(12) from user group by id) t
		sel3 := &sqlparser.Select{
			SelectExprs: countStartExpr,
			From: []sqlparser.TableExpr{
				&sqlparser.AliasedTableExpr{
					Expr: &sqlparser.Subquery{Select: sel2},
					As:   sqlparser.NewTableIdent("t"),
				},
			},
		}
		sel2 = sel3
	}

	cjt := newJointab(sqlparser.GetBindvars(sel2))
	countpb := newPrimitiveBuilder(vschema, cjt)
	err = countpb.processSelect(sel2, outer, "")
	if err != nil {
		return nil, err
	}
	return &sqlCalcFoundRows{LimitQuery: frpb.bldr, CountQuery: countpb.bldr, ljt: ljt, cjt: cjt}, nil
}

func handleDualSelects(sel *sqlparser.Select, vschema ContextVSchema) (engine.Primitive, error) {
	if !isOnlyDual(sel) {
		return nil, nil
	}

	exprs := make([]evalengine.Expr, len(sel.SelectExprs))
	cols := make([]string, len(sel.SelectExprs))
	for i, e := range sel.SelectExprs {
		expr, ok := e.(*sqlparser.AliasedExpr)
		if !ok {
			return nil, nil
		}
		var err error
		if sqlparser.IsLockingFunc(expr.Expr) {
			// if we are using any locking functions, we bail out here and send the whole query to a single destination
			return buildLockingPrimitive(sel, vschema)

		}
		exprs[i], err = sqlparser.Convert(expr.Expr)
		if err != nil {
			return nil, nil
		}
		cols[i] = expr.As.String()
		if cols[i] == "" {
			cols[i] = sqlparser.String(expr.Expr)
		}
	}
	return &engine.Projection{
		Exprs: exprs,
		Cols:  cols,
		Input: &engine.SingleRow{},
	}, nil
}

func buildLockingPrimitive(sel *sqlparser.Select, vschema ContextVSchema) (engine.Primitive, error) {
	ks, err := vschema.FirstSortedKeyspace()
	if err != nil {
		return nil, err
	}
	return &engine.Lock{
		Keyspace:          ks,
		TargetDestination: key.DestinationKeyspaceID{0},
		Query:             sqlparser.String(sel),
	}, nil
}

func isOnlyDual(sel *sqlparser.Select) bool {
	if sel.Where != nil || sel.GroupBy != nil || sel.Having != nil || sel.Limit != nil || sel.OrderBy != nil {
		// we can only deal with queries without any other subclauses - just SELECT and FROM, nothing else is allowed
		return false
	}

	if len(sel.From) > 1 {
		return false
	}
	table, ok := sel.From[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return false
	}
	tableName, ok := table.Expr.(sqlparser.TableName)

	return ok && tableName.Name.String() == "dual" && tableName.Qualifier.IsEmpty()
}

// pushFilter identifies the target route for the specified bool expr,
// pushes it down, and updates the route info if the new constraint improves
// the primitive. This function can push to a WHERE or HAVING clause.
func (pb *primitiveBuilder) pushFilter(in sqlparser.Expr, whereType string) error {
	filters := splitAndExpression(nil, in)
	reorderBySubquery(filters)
	for _, filter := range filters {
		pullouts, origin, expr, err := pb.findOrigin(filter)
		if err != nil {
			return err
		}
		rut, isRoute := origin.(*route)
		if isRoute && rut.eroute.Opcode == engine.SelectDBA {
			schemaNameExpr, err := rewriteTableSchema(expr)
			if err != nil {
				return err
			}
			if schemaNameExpr != nil {
				if rut.eroute.SysTableKeyspaceExpr != nil {
					return vterrors.Errorf(vtrpcpb.Code_INVALID_ARGUMENT, "two predicates for table_schema not supported")
				}
				rut.eroute.SysTableKeyspaceExpr = schemaNameExpr
			}
		}
		// The returned expression may be complex. Resplit before pushing.
		for _, subexpr := range splitAndExpression(nil, expr) {
			if err := pb.bldr.PushFilter(pb, subexpr, whereType, origin); err != nil {
				return err
			}
		}
		pb.addPullouts(pullouts)
	}
	return nil
}

func findOtherComparator(cmp *sqlparser.ComparisonExpr) (sqlparser.Expr, sqlparser.Expr, func(arg sqlparser.Argument)) {
	if isTableSchema(cmp.Left) {
		return cmp.Left, cmp.Right, func(arg sqlparser.Argument) {
			cmp.Left = arg
		}
	}
	if isTableSchema(cmp.Right) {
		return cmp.Right, cmp.Left, func(arg sqlparser.Argument) {
			cmp.Right = arg
		}
	}

	return nil, nil, nil
}

func isTableSchema(e sqlparser.Expr) bool {
	col, ok := e.(*sqlparser.ColName)
	if !ok {
		return false
	}
	return col.Name.EqualString("table_schema")
}

func rewriteTableSchema(in sqlparser.Expr) (evalengine.Expr, error) {
	switch cmp := in.(type) {
	case *sqlparser.ComparisonExpr:
		if cmp.Operator == sqlparser.EqualOp {
			schemaName, other, replaceOther := findOtherComparator(cmp)

			if schemaName != nil && shouldRewrite(other) {
				evalExpr, err := sqlparser.Convert(other)
				if err != nil {
					if err == sqlparser.ErrExprNotSupported {
						// This just means we can't rewrite this particular expression,
						// not that we have to exit altogether
						return nil, nil
					}
					return nil, err
				}
				replaceOther(sqlparser.NewArgument([]byte(":" + sqltypes.BvSchemaName)))
				return evalExpr, nil
			}
		}
	}
	return nil, nil
}

func shouldRewrite(e sqlparser.Expr) bool {
	switch node := e.(type) {
	case *sqlparser.FuncExpr:
		// we should not rewrite database() calls against information_schema
		return !(node.Name.EqualString("database") || node.Name.EqualString("schema"))
	}
	return true
}

// reorderBySubquery reorders the filters by pushing subqueries
// to the end. This allows the non-subquery filters to be
// pushed first because they can potentially improve the routing
// plan, which can later allow a filter containing a subquery
// to successfully merge with the corresponding route.
func reorderBySubquery(filters []sqlparser.Expr) {
	max := len(filters)
	for i := 0; i < max; i++ {
		if !hasSubquery(filters[i]) {
			continue
		}
		saved := filters[i]
		for j := i; j < len(filters)-1; j++ {
			filters[j] = filters[j+1]
		}
		filters[len(filters)-1] = saved
		max--
	}
}

// addPullouts adds the pullout subqueries to the primitiveBuilder.
func (pb *primitiveBuilder) addPullouts(pullouts []*pulloutSubquery) {
	for _, pullout := range pullouts {
		pullout.setUnderlying(pb.bldr)
		pb.bldr = pullout
		pb.bldr.Reorder(0)
	}
}

// pushSelectExprs identifies the target route for the
// select expressions and pushes them down.
func (pb *primitiveBuilder) pushSelectExprs(sel *sqlparser.Select) error {
	resultColumns, err := pb.pushSelectRoutes(sel.SelectExprs)
	if err != nil {
		return err
	}
	pb.st.SetResultColumns(resultColumns)
	return pb.pushGroupBy(sel)
}

// pushSelectRoutes is a convenience function that pushes all the select
// expressions and returns the list of resultColumns generated for it.
func (pb *primitiveBuilder) pushSelectRoutes(selectExprs sqlparser.SelectExprs) ([]*resultColumn, error) {
	resultColumns := make([]*resultColumn, 0, len(selectExprs))
	for _, node := range selectExprs {
		switch node := node.(type) {
		case *sqlparser.AliasedExpr:
			pullouts, origin, expr, err := pb.findOrigin(node.Expr)
			if err != nil {
				return nil, err
			}
			node.Expr = expr
			rc, _, err := pb.bldr.PushSelect(pb, node, origin)
			if err != nil {
				return nil, err
			}
			resultColumns = append(resultColumns, rc)
			pb.addPullouts(pullouts)
		case *sqlparser.StarExpr:
			var expanded bool
			var err error
			resultColumns, expanded, err = pb.expandStar(resultColumns, node)
			if err != nil {
				return nil, err
			}
			if expanded {
				continue
			}
			// We'll allow select * for simple routes.
			rb, ok := pb.bldr.(*route)
			if !ok {
				return nil, errors.New("unsupported: '*' expression in cross-shard query")
			}
			// Validate keyspace reference if any.
			if !node.TableName.IsEmpty() {
				if _, err := pb.st.FindTable(node.TableName); err != nil {
					return nil, err
				}
			}
			resultColumns = append(resultColumns, rb.PushAnonymous(node))
		case sqlparser.Nextval:
			rb, ok := pb.bldr.(*route)
			if !ok {
				// This code is unreachable because the parser doesn't allow joins for next val statements.
				return nil, errors.New("unsupported: SELECT NEXT query in cross-shard query")
			}
			if rb.eroute.Opcode != engine.SelectNext {
				return nil, errors.New("NEXT used on a non-sequence table")
			}
			rb.eroute.Opcode = engine.SelectNext
			resultColumns = append(resultColumns, rb.PushAnonymous(node))
		default:
			return nil, fmt.Errorf("BUG: unexpected select expression type: %T", node)
		}
	}
	return resultColumns, nil
}

// expandStar expands a StarExpr and pushes the expanded
// expressions down if the tables have authoritative column lists.
// If not, it returns false.
// This function breaks the abstraction a bit: it directly sets the
// the Metadata for newly created expressions. In all other cases,
// the Metadata is set through a symtab Find.
func (pb *primitiveBuilder) expandStar(inrcs []*resultColumn, expr *sqlparser.StarExpr) (outrcs []*resultColumn, expanded bool, err error) {
	tables := pb.st.AllTables()
	if tables == nil {
		// no table metadata available.
		return inrcs, false, nil
	}
	if expr.TableName.IsEmpty() {
		for _, t := range tables {
			// All tables must have authoritative column lists.
			if !t.isAuthoritative {
				return inrcs, false, nil
			}
		}
		singleTable := false
		if len(tables) == 1 {
			singleTable = true
		}
		for _, t := range tables {
			for _, col := range t.columnNames {
				var expr *sqlparser.AliasedExpr
				if singleTable {
					// If there's only one table, we use unqualified column names.
					expr = &sqlparser.AliasedExpr{
						Expr: &sqlparser.ColName{
							Metadata: t.columns[col.Lowered()],
							Name:     col,
						},
					}
				} else {
					// If a and b have id as their column, then
					// select * from a join b should result in
					// select a.id as id, b.id as id from a join b.
					expr = &sqlparser.AliasedExpr{
						Expr: &sqlparser.ColName{
							Metadata:  t.columns[col.Lowered()],
							Name:      col,
							Qualifier: t.alias,
						},
						As: col,
					}
				}
				rc, _, err := pb.bldr.PushSelect(pb, expr, t.Origin())
				if err != nil {
					// Unreachable because PushSelect won't fail on ColName.
					return inrcs, false, err
				}
				inrcs = append(inrcs, rc)
			}
		}
		return inrcs, true, nil
	}

	// Expression qualified with table name.
	t, err := pb.st.FindTable(expr.TableName)
	if err != nil {
		return inrcs, false, err
	}
	if !t.isAuthoritative {
		return inrcs, false, nil
	}
	for _, col := range t.columnNames {
		expr := &sqlparser.AliasedExpr{
			Expr: &sqlparser.ColName{
				Metadata:  t.columns[col.Lowered()],
				Name:      col,
				Qualifier: expr.TableName,
			},
		}
		rc, _, err := pb.bldr.PushSelect(pb, expr, t.Origin())
		if err != nil {
			// Unreachable because PushSelect won't fail on ColName.
			return inrcs, false, err
		}
		inrcs = append(inrcs, rc)
	}
	return inrcs, true, nil
}
