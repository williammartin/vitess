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
	querypb "vitess.io/vitess/go/vt/proto/query"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/operators"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
)

func gen4InsertStmtPlanner(version querypb.ExecuteOptions_PlannerVersion, insStmt *sqlparser.Insert, reservedVars *sqlparser.ReservedVars, vschema plancontext.VSchema) (*planResult, error) {
	ctx, err := plancontext.CreatePlanningContext(insStmt, reservedVars, vschema, version)
	if err != nil {
		return nil, err
	}

	err = rewriteRoutedTables(insStmt, vschema)
	if err != nil {
		return nil, err
	}
	// remove any alias added from routing table.
	// insert query does not support table alias.
	insStmt.Table.As = sqlparser.NewIdentifierCS("")

	// Check single unsharded. Even if the table is for single unsharded but sequence table is used.
	// We cannot shortcut here as sequence column needs additional planning.
	ks, tables := ctx.SemTable.SingleUnshardedKeyspace()
	// Remove all the foreign keys that don't require any handling.
	err = ctx.SemTable.RemoveNonRequiredForeignKeys(ctx.VerifyAllFKs, vindexes.UpdateAction)
	if err != nil {
		return nil, err
	}
	if ks != nil {
		if tables[0].AutoIncrement == nil && !ctx.SemTable.ForeignKeysPresent() {
			plan := insertUnshardedShortcut(insStmt, ks, tables)
			setCommentDirectivesOnPlan(plan, insStmt)
			return newPlanResult(plan.Primitive(), operators.QualifiedTables(ks, tables)...), nil
		}
	}

	tblInfo, err := ctx.SemTable.TableInfoFor(ctx.SemTable.TableSetFor(insStmt.Table))
	if err != nil {
		return nil, err
	}

	if err = errOutIfPlanCannotBeConstructed(ctx, tblInfo.GetVindexTable(), insStmt, ctx.SemTable.ForeignKeysPresent()); err != nil {
		return nil, err
	}

	err = queryRewrite(ctx.SemTable, reservedVars, insStmt)
	if err != nil {
		return nil, err
	}

	op, err := operators.PlanQuery(ctx, insStmt)
	if err != nil {
		return nil, err
	}

	plan, err := transformToLogicalPlan(ctx, op)
	if err != nil {
		return nil, err
	}

	return newPlanResult(plan.Primitive(), operators.TablesUsed(op)...), nil
}

func errOutIfPlanCannotBeConstructed(ctx *plancontext.PlanningContext, vTbl *vindexes.Table, insStmt *sqlparser.Insert, fkPlanNeeded bool) error {
	if vTbl.Keyspace.Sharded && ctx.SemTable.NotUnshardedErr != nil {
		return ctx.SemTable.NotUnshardedErr
	}
	if insStmt.Action != sqlparser.ReplaceAct {
		return nil
	}
	if fkPlanNeeded {
		return vterrors.VT12001("REPLACE INTO with foreign keys")
	}
	return nil
}

func insertUnshardedShortcut(stmt *sqlparser.Insert, ks *vindexes.Keyspace, tables []*vindexes.Table) logicalPlan {
	eIns := &engine.Insert{}
	eIns.Keyspace = ks
	eIns.TableName = tables[0].Name.String()
	eIns.Opcode = engine.InsertUnsharded
	eIns.Query = generateQuery(stmt)
	return &insert{eInsert: eIns}
}

type insert struct {
	eInsert *engine.Insert
	source  logicalPlan
}

var _ logicalPlan = (*insert)(nil)

func (i *insert) Primitive() engine.Primitive {
	if i.source != nil {
		i.eInsert.Input = i.source.Primitive()
	}
	return i.eInsert
}

func (i *insert) ContainsTables() semantics.TableSet {
	panic("does not expect insert to get contains tables call")
}
