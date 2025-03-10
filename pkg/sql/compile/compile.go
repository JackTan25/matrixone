// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compile

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/cnservice/cnclient"
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/common/mpool"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/defines"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/pb/pipeline"
	"github.com/matrixorigin/matrixone/pkg/pb/plan"
	"github.com/matrixorigin/matrixone/pkg/pb/timestamp"
	"github.com/matrixorigin/matrixone/pkg/perfcounter"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/connector"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/deletion"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/dispatch"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/external"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/insert"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/merge"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/mergeblock"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/mergedelete"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec/output"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
	plan2 "github.com/matrixorigin/matrixone/pkg/sql/plan"
	"github.com/matrixorigin/matrixone/pkg/sql/util"
	"github.com/matrixorigin/matrixone/pkg/util/trace"
	"github.com/matrixorigin/matrixone/pkg/vm"
	"github.com/matrixorigin/matrixone/pkg/vm/engine"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

// Note: Now the cost going from stat is actually the number of rows, so we can only estimate a number for the size of each row.
// The current insertion of around 200,000 rows triggers cn to write s3 directly
const (
	DistributedThreshold   uint64 = 10 * mpool.MB
	SingleLineSizeEstimate uint64 = 300 * mpool.B
)

// New is used to new an object of compile
func New(addr, db string, sql string, tenant, uid string, ctx context.Context,
	e engine.Engine, proc *process.Process, stmt tree.Statement, isInternal bool, cnLabel map[string]string) *Compile {
	return &Compile{
		e:          e,
		db:         db,
		ctx:        ctx,
		tenant:     tenant,
		uid:        uid,
		sql:        sql,
		proc:       proc,
		stmt:       stmt,
		addr:       addr,
		stepRegs:   make(map[int32][]*process.WaitRegister),
		isInternal: isInternal,
		cnLabel:    cnLabel,
	}
}

// helper function to judge if init temporary engine is needed
func (c *Compile) NeedInitTempEngine(InitTempEngine bool) bool {
	if InitTempEngine {
		return false
	}
	for _, s := range c.scope {
		ddl := s.Plan.GetDdl()
		if ddl == nil {
			continue
		}
		if qry := ddl.GetCreateTable(); qry != nil && qry.Temporary {
			if c.e.(*engine.EntireEngine).TempEngine == nil {
				return true
			}
		}
	}
	return false
}

func (c *Compile) SetTempEngine(ctx context.Context, te engine.Engine) {
	e := c.e.(*engine.EntireEngine)
	e.TempEngine = te
	c.ctx = ctx
}

// Compile is the entrance of the compute-execute-layer.
// It generates a scope (logic pipeline) for a query plan.
func (c *Compile) Compile(ctx context.Context, pn *plan.Plan, u any, fill func(any, *batch.Batch) error) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = moerr.ConvertPanicError(ctx, e)
		}
	}()
	// with values
	c.proc.Ctx = perfcounter.WithCounterSet(c.proc.Ctx, &c.s3CounterSet)
	c.ctx = c.proc.Ctx

	// session info and callback function to write back query result.
	// XXX u is really a bad name, I'm not sure if `session` or `user` will be more suitable.
	c.u = u
	c.fill = fill

	// get execute related information
	// about ap or tp, what and how many compute resource we can use.
	c.info = plan2.GetExecTypeFromPlan(pn)

	// Compile may exec some function that need engine.Engine.
	c.proc.Ctx = context.WithValue(c.proc.Ctx, defines.EngineKey{}, c.e)
	// generate logic pipeline for query.
	c.scope, err = c.compileScope(ctx, pn)
	if err != nil {
		return err
	}
	for _, s := range c.scope {
		if len(s.NodeInfo.Addr) == 0 {
			s.NodeInfo.Addr = c.addr
		}
	}
	return nil
}

func (c *Compile) setAffectedRows(n uint64) {
	c.affectRows = n
}

func (c *Compile) GetAffectedRows() uint64 {
	return c.affectRows
}

func (c *Compile) run(s *Scope) error {
	if s == nil {
		return nil
	}

	switch s.Magic {
	case Normal:
		defer c.fillAnalyzeInfo()
		return s.Run(c)
	case Merge:
		defer c.fillAnalyzeInfo()
		return s.MergeRun(c)
	case MergeDelete:
		defer c.fillAnalyzeInfo()
		err := s.MergeRun(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(s.Instructions[len(s.Instructions)-1].Arg.(*mergedelete.Argument).AffectedRows)
		return nil
	case MergeInsert:
		defer c.fillAnalyzeInfo()
		err := s.MergeRun(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(s.Instructions[len(s.Instructions)-1].Arg.(*mergeblock.Argument).AffectedRows)
		return nil
	case Remote:
		defer c.fillAnalyzeInfo()
		return s.RemoteRun(c)
	case CreateDatabase:
		err := s.CreateDatabase(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(1)
		return nil
	case DropDatabase:
		err := s.DropDatabase(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(1)
		return nil
	case CreateTable:
		qry := s.Plan.GetDdl().GetCreateTable()
		if qry.Temporary {
			return s.CreateTempTable(c)
		} else {
			return s.CreateTable(c)
		}
	case AlterView:
		return s.AlterView(c)
	case AlterTable:
		return s.AlterTable(c)
	case DropTable:
		return s.DropTable(c)
	case DropSequence:
		return s.DropSequence(c)
	case CreateSequence:
		return s.CreateSequence(c)
	case CreateIndex:
		return s.CreateIndex(c)
	case DropIndex:
		return s.DropIndex(c)
	case TruncateTable:
		return s.TruncateTable(c)
	case Deletion:
		defer c.fillAnalyzeInfo()
		affectedRows, err := s.Delete(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(affectedRows)
		return nil
	case Insert:
		defer c.fillAnalyzeInfo()
		affectedRows, err := s.Insert(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(affectedRows)
		return nil
	case Update:
		defer c.fillAnalyzeInfo()
		affectedRows, err := s.Update(c)
		if err != nil {
			return err
		}
		c.setAffectedRows(affectedRows)
		return nil
	}
	return nil
}

// Run is an important function of the compute-layer, it executes a single sql according to its scope
func (c *Compile) Run(_ uint64) error {
	var wg sync.WaitGroup
	errC := make(chan error, len(c.scope))
	for _, s := range c.scope {
		wg.Add(1)
		go func(scope *Scope) {
			errC <- c.run(scope)
			wg.Done()
		}(s)
	}
	go func() {
		wg.Wait()
		c.scope = nil
		close(errC)
	}()
	for e := range errC {
		if e != nil {
			return e
		}
	}
	c.proc.FreeVectors()
	return nil
}

func (c *Compile) compileScope(ctx context.Context, pn *plan.Plan) ([]*Scope, error) {
	switch qry := pn.Plan.(type) {
	case *plan.Plan_Query:
		scopes, err := c.compileQuery(ctx, qry.Query)
		if err != nil {
			return nil, err
		}
		for _, s := range scopes {
			s.Plan = pn
		}
		return scopes, nil
	case *plan.Plan_Ddl:
		switch qry.Ddl.DdlType {
		case plan.DataDefinition_CREATE_DATABASE:
			return []*Scope{{
				Magic: CreateDatabase,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_DROP_DATABASE:
			var preScopes []*Scope
			var err error
			if pn.AttachedPlan != nil {
				preScopes, err = c.compileAttachedScope(ctx, pn.AttachedPlan)
				if err != nil {
					return nil, err
				}
			}
			return []*Scope{{
				Magic:     DropDatabase,
				Plan:      pn,
				PreScopes: preScopes,
			}}, nil
		case plan.DataDefinition_CREATE_TABLE:
			return []*Scope{{
				Magic: CreateTable,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_ALTER_VIEW:
			return []*Scope{{
				Magic: AlterView,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_ALTER_TABLE:
			var preScopes []*Scope
			var err error
			if pn.AttachedPlan != nil {
				preScopes, err = c.compileAttachedScope(ctx, pn.AttachedPlan)
				if err != nil {
					return nil, err
				}
			}
			return []*Scope{{
				Magic:     AlterTable,
				Plan:      pn,
				PreScopes: preScopes,
			}}, nil
		case plan.DataDefinition_DROP_TABLE:
			var preScopes []*Scope
			var err error
			if pn.AttachedPlan != nil {
				preScopes, err = c.compileAttachedScope(ctx, pn.AttachedPlan)
				if err != nil {
					return nil, err
				}
			}
			return []*Scope{{
				Magic:     DropTable,
				Plan:      pn,
				PreScopes: preScopes,
			}}, nil
		case plan.DataDefinition_DROP_SEQUENCE:
			return []*Scope{{
				Magic: DropSequence,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_TRUNCATE_TABLE:
			return []*Scope{{
				Magic: TruncateTable,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_CREATE_SEQUENCE:
			return []*Scope{{
				Magic: CreateSequence,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_CREATE_INDEX:
			return []*Scope{{
				Magic: CreateIndex,
				Plan:  pn,
			}}, nil
		case plan.DataDefinition_DROP_INDEX:
			var preScopes []*Scope
			var err error
			if pn.AttachedPlan != nil {
				preScopes, err = c.compileAttachedScope(ctx, pn.AttachedPlan)
				if err != nil {
					return nil, err
				}
			}
			return []*Scope{{
				Magic:     DropIndex,
				Plan:      pn,
				PreScopes: preScopes,
			}}, nil
		case plan.DataDefinition_SHOW_DATABASES,
			plan.DataDefinition_SHOW_TABLES,
			plan.DataDefinition_SHOW_COLUMNS,
			plan.DataDefinition_SHOW_CREATETABLE:
			return c.compileQuery(ctx, pn.GetDdl().GetQuery())
			// 1、not supported: show arnings/errors/status/processlist
			// 2、show variables will not return query
			// 3、show create database/table need rewrite to create sql
		}
	}
	return nil, moerr.NewNYI(ctx, fmt.Sprintf("query '%s'", pn))
}

func (c *Compile) cnListStrategy() {
	if len(c.cnList) == 0 {
		c.cnList = append(c.cnList, engine.Node{
			Addr: c.addr,
			Mcpu: c.NumCPU(),
		})
	} else if len(c.cnList) > c.info.CnNumbers {
		c.cnList = c.cnList[:c.info.CnNumbers]
	}
}

func (c *Compile) compileAttachedScope(ctx context.Context, attachedPlan *plan.Plan) ([]*Scope, error) {
	query := attachedPlan.Plan.(*plan.Plan_Query)
	attachedScope, err := c.compileQuery(ctx, query.Query)
	if err != nil {
		return nil, err
	}
	for _, s := range attachedScope {
		s.Plan = attachedPlan
	}
	return attachedScope, nil
}

func (c *Compile) compileQuery(ctx context.Context, qry *plan.Query) ([]*Scope, error) {
	var err error
	c.cnList, err = c.e.Nodes(c.isInternal, c.tenant, c.cnLabel)
	if err != nil {
		return nil, err
	}
	if c.info.Typ == plan2.ExecTypeAP {
		client := cnclient.GetRPCClient()
		if client != nil {
			for i := 0; i < len(c.cnList); i++ {
				_, _, err := net.SplitHostPort(c.cnList[i].Addr)
				if err != nil {
					logutil.Warnf("compileScope received a malformed cn address '%s', expected 'ip:port'", c.cnList[i].Addr)
				}
				if isSameCN(c.addr, c.cnList[i].Addr) {
					continue
				}
				logutil.Infof("ping start")
				err = client.Ping(ctx, c.cnList[i].Addr)
				logutil.Infof("ping err %+v\n", err)
				// ping failed
				if err != nil {
					logutil.Infof("ping err %+v\n", err)
					c.cnList = append(c.cnList[:i], c.cnList[i+1:]...)
					i--
				}
			}
		}
	}

	c.info.CnNumbers = len(c.cnList)
	blkNum := 0
	for _, n := range qry.Nodes {
		if n.NodeType == plan.Node_TABLE_SCAN {
			if n.Stats != nil {
				blkNum += int(n.Stats.BlockNum)
			}
		}
	}
	switch qry.StmtType {
	case plan.Query_INSERT:
		insertNode := qry.Nodes[qry.Steps[0]]
		nodeStats := qry.Nodes[insertNode.Children[0]].Stats
		if nodeStats.GetCost()*float64(SingleLineSizeEstimate) > float64(DistributedThreshold) || qry.LoadTag || blkNum >= MinBlockNum {
			if len(insertNode.InsertCtx.OnDuplicateIdx) > 0 {
				c.cnList = engine.Nodes{
					engine.Node{
						Addr: c.addr,
						Mcpu: c.generateCPUNumber(1, blkNum)},
				}
			} else {
				c.cnListStrategy()
			}
		} else {
			if len(insertNode.InsertCtx.OnDuplicateIdx) > 0 {
				c.cnList = engine.Nodes{
					engine.Node{
						Addr: c.addr,
						Mcpu: c.generateCPUNumber(1, blkNum)},
				}
			} else {
				c.cnList = engine.Nodes{engine.Node{
					Addr: c.addr,
					Mcpu: c.generateCPUNumber(c.NumCPU(), blkNum)},
				}
			}
		}
	default:
		if blkNum < MinBlockNum {
			c.cnList = engine.Nodes{engine.Node{
				Addr: c.addr,
				Mcpu: c.generateCPUNumber(c.NumCPU(), blkNum)},
			}
		} else {
			c.cnListStrategy()
		}
	}

	c.initAnalyze(qry)

	steps := make([]*Scope, 0, len(qry.Steps))
	for i := len(qry.Steps) - 1; i >= 0; i-- {
		scopes, err := c.compilePlanScope(ctx, int32(i), qry.Steps[i], qry.Nodes)
		if err != nil {
			return nil, err
		}
		scope, err := c.compileApQuery(qry, scopes)
		if err != nil {
			return nil, err
		}
		steps = append(steps, scope)
	}

	return steps, err
}

// for now, cn-write-s3 delete just support
func IsSingleDelete(delCtx *deletion.DeleteCtx) bool {
	if len(delCtx.IdxIdx) > 0 || len(delCtx.IdxSource) > 0 {
		return false
	}

	if len(delCtx.OnRestrictIdx) > 0 {
		return false
	}

	if len(delCtx.OnCascadeIdx) > 0 || len(delCtx.OnCascadeSource) > 0 {
		return false
	}

	if len(delCtx.OnSetIdx) > 0 || len(delCtx.OnSetSource) > 0 {
		return false
	}

	if len(delCtx.OnSetUniqueSource) > 0 && delCtx.OnSetUniqueSource[0] != nil || len(delCtx.OnSetRef) > 0 {
		return false
	}

	if len(delCtx.OnSetTableDef) > 0 || len(delCtx.OnSetUpdateCol) > 0 {
		return false
	}

	if len(delCtx.DelSource) > 1 {
		return false
	}
	return true
}

func (c *Compile) compileApQuery(qry *plan.Query, ss []*Scope) (*Scope, error) {
	var rs *Scope
	switch qry.StmtType {
	case plan.Query_DELETE:
		deleteNode := qry.Nodes[qry.Steps[0]]
		deleteNode.NotCacheable = true
		nodeStats := qry.Nodes[deleteNode.Children[0]].Stats
		// 1.Estiminate the cost of delete rows, if we need to delete too much rows,
		// just use distributed deletion (write s3 delete just think about delete
		// single table)
		arg, err := constructDeletion(qry.Nodes[qry.Steps[0]], c.e, c.proc)
		if err != nil {
			return nil, err
		}
		if nodeStats.GetCost()*float64(SingleLineSizeEstimate) > float64(DistributedThreshold) && IsSingleDelete(arg.DeleteCtx) && !arg.DeleteCtx.CanTruncate {
			rs = c.newDeleteMergeScope(arg, ss)
			rs.Instructions = append(rs.Instructions, vm.Instruction{
				Op: vm.MergeDelete,
				Arg: &mergedelete.Argument{
					DelSource: arg.DeleteCtx.DelSource[0],
				},
			})
			rs.Magic = MergeDelete
		} else {
			rs = c.newMergeScope(ss)
			updateScopesLastFlag([]*Scope{rs})
			rs.Magic = Deletion
			c.setAnalyzeCurrent([]*Scope{rs}, c.anal.curr)
			if err != nil {
				return nil, err
			}
			rs.Instructions = append(rs.Instructions, vm.Instruction{
				Op:  vm.Deletion,
				Arg: arg,
			})
		}
	case plan.Query_INSERT:
		insertNode := qry.Nodes[qry.Steps[0]]
		insertNode.NotCacheable = true

		preArg, err := constructPreInsert(insertNode, c.e, c.proc)
		if err != nil {
			return nil, err
		}

		arg, err := constructInsert(insertNode, c.e, c.proc)
		if err != nil {
			return nil, err
		}
		nodeStats := qry.Nodes[insertNode.Children[0]].Stats

		if nodeStats.GetCost()*float64(SingleLineSizeEstimate) > float64(DistributedThreshold) || qry.LoadTag {
			// use distributed-insert
			arg.IsRemote = true
			for _, scope := range ss {
				scope.Instructions = append(scope.Instructions, vm.Instruction{
					Op:  vm.PreInsert,
					Arg: preArg,
				})
			}

			rs = c.newInsertMergeScope(arg, ss)
			rs.Magic = MergeInsert
			rs.Instructions = append(rs.Instructions, vm.Instruction{
				Op: vm.MergeBlock,
				Arg: &mergeblock.Argument{
					Tbl:         arg.InsertCtx.Rels[0],
					Unique_tbls: arg.InsertCtx.Rels[1:],
				},
			})
		} else {
			rs = c.newMergeScope(ss)
			rs.Magic = Insert
			c.setAnalyzeCurrent([]*Scope{rs}, c.anal.curr)
			if len(insertNode.InsertCtx.OnDuplicateIdx) > 0 {
				onDuplicateKeyArg, err := constructOnduplicateKey(insertNode, c.e, c.proc)
				if err != nil {
					return nil, err
				}
				rs.Instructions = append(rs.Instructions, vm.Instruction{
					Op:  vm.OnDuplicateKey,
					Arg: onDuplicateKeyArg,
				})
			}
			rs.Instructions = append(rs.Instructions, vm.Instruction{
				Op:  vm.PreInsert,
				Arg: preArg,
			})
			rs.Instructions = append(rs.Instructions, vm.Instruction{
				Op:  vm.Insert,
				Arg: arg,
			})
		}
	case plan.Query_UPDATE:
		scp, err := constructUpdate(qry.Nodes[qry.Steps[0]], c.e, c.proc)
		if err != nil {
			return nil, err
		}
		rs = c.newMergeScope(ss)
		updateScopesLastFlag([]*Scope{rs})
		rs.Magic = Update
		c.setAnalyzeCurrent([]*Scope{rs}, c.anal.curr)
		rs.Instructions = append(rs.Instructions, vm.Instruction{
			Op:  vm.Update,
			Arg: scp,
		})
	default:
		rs = c.newMergeScope(ss)
		updateScopesLastFlag([]*Scope{rs})
		c.setAnalyzeCurrent([]*Scope{rs}, c.anal.curr)
		rs.Instructions = append(rs.Instructions, vm.Instruction{
			Op: vm.Output,
			Arg: &output.Argument{
				Data: c.u,
				Func: c.fill,
			},
		})
	}
	return rs, nil
}

func constructValueScanBatch(ctx context.Context, proc *process.Process, node *plan.Node) (*batch.Batch, error) {
	if node == nil || node.TableDef == nil { // like : select 1, 2
		bat := batch.NewWithSize(1)
		bat.Vecs[0] = vector.NewConstNull(types.T_int64.ToType(), 1, proc.Mp())
		bat.InitZsOne(1)
		return bat, nil
	}
	// select * from (values row(1,1), row(2,2), row(3,3)) a;
	tableDef := node.TableDef
	colCount := len(tableDef.Cols)
	colsData := node.RowsetData.Cols
	rowCount := len(colsData[0].Data)
	bat := batch.NewWithSize(colCount)

	for i := 0; i < colCount; i++ {
		vec, err := rowsetDataToVector(ctx, proc, colsData[i].Data)
		if err != nil {
			return nil, err
		}
		bat.Vecs[i] = vec
	}
	bat.SetZs(rowCount, proc.Mp())
	return bat, nil
}

func (c *Compile) compilePlanScope(ctx context.Context, step int32, curNodeIdx int32, ns []*plan.Node) ([]*Scope, error) {
	n := ns[curNodeIdx]
	switch n.NodeType {
	case plan.Node_VALUE_SCAN:
		bat := c.proc.GetPrepareBatch()
		if bat == nil {
			var err error

			if bat, err = constructValueScanBatch(ctx, c.proc, n); err != nil {
				return nil, err
			}
		}
		ds := &Scope{
			Magic:      Normal,
			DataSource: &Source{Bat: bat},
			NodeInfo:   engine.Node{Addr: c.addr, Mcpu: 1},
			Proc:       process.NewWithAnalyze(c.proc, c.ctx, 0, c.anal.Nodes()),
		}
		return c.compileSort(n, c.compileProjection(n, []*Scope{ds})), nil
	case plan.Node_EXTERNAL_SCAN:
		node := plan2.DeepCopyNode(n)
		ss, err := c.compileExternScan(ctx, node)
		if err != nil {
			return nil, err
		}
		return c.compileSort(n, c.compileProjection(n, c.compileRestrict(node, ss))), nil
	case plan.Node_TABLE_SCAN:
		ss, err := c.compileTableScan(n)
		if err != nil {
			return nil, err
		}

		// RelationName
		return c.compileSort(n, c.compileProjection(n, c.compileRestrict(n, ss))), nil
	case plan.Node_FILTER, plan.Node_PROJECT:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		ss, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(ss, curr)
		return c.compileSort(n, c.compileProjection(n, c.compileRestrict(n, ss))), nil
	case plan.Node_AGG:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		ss, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(ss, curr)

		if idx := plan2.GetShuffleIndexForGroupBy(n); idx >= 0 {
			ss = c.compileBucketGroup(n, ss, ns, idx)
			return c.compileSort(n, ss), nil
		} else {
			ss = c.compileMergeGroup(n, ss, ns)
			return c.compileSort(n, c.compileProjection(n, c.compileRestrict(n, ss))), nil
		}

	case plan.Node_JOIN:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		left, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(left, int(n.Children[1]))
		right, err := c.compilePlanScope(ctx, step, n.Children[1], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(right, curr)
		return c.compileSort(n, c.compileJoin(ctx, n, ns[n.Children[0]], ns[n.Children[1]], left, right)), nil
	case plan.Node_SORT:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		ss, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(ss, curr)
		return c.compileProjection(n, c.compileRestrict(n, c.compileSort(n, ss))), nil
	case plan.Node_UNION:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		left, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(left, int(n.Children[1]))
		right, err := c.compilePlanScope(ctx, step, n.Children[1], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(right, curr)
		return c.compileSort(n, c.compileUnion(n, left, right)), nil
	case plan.Node_MINUS, plan.Node_INTERSECT, plan.Node_INTERSECT_ALL:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		left, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(left, int(n.Children[1]))
		right, err := c.compilePlanScope(ctx, step, n.Children[1], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(right, curr)
		return c.compileSort(n, c.compileMinusAndIntersect(n, left, right, n.NodeType)), nil
	case plan.Node_UNION_ALL:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		left, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(left, int(n.Children[1]))
		right, err := c.compilePlanScope(ctx, step, n.Children[1], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(right, curr)
		return c.compileSort(n, c.compileUnionAll(left, right)), nil
	case plan.Node_DELETE:
		if n.DeleteCtx.CanTruncate {
			return nil, nil
		}
		return c.compilePlanScope(ctx, step, n.Children[0], ns)
	case plan.Node_INSERT, plan.Node_UPDATE:
		return c.compilePlanScope(ctx, step, n.Children[0], ns)
	case plan.Node_FUNCTION_SCAN:
		curr := c.anal.curr
		c.setAnalyzeCurrent(nil, int(n.Children[0]))
		ss, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		c.setAnalyzeCurrent(ss, curr)
		return c.compileSort(n, c.compileProjection(n, c.compileRestrict(n, c.compileTableFunction(n, ss)))), nil
	case plan.Node_SINK_SCAN:
		rs := &Scope{
			Magic:    Merge,
			NodeInfo: engine.Node{Addr: c.addr, Mcpu: c.NumCPU()},
			Proc:     process.NewWithAnalyze(c.proc, c.ctx, 1, c.anal.Nodes()),
		}
		c.appendStepRegs(n.SourceStep, rs.Proc.Reg.MergeReceivers[0])
		return []*Scope{rs}, nil
	case plan.Node_SINK:
		receivers, ok := c.getStepRegs(step)
		if !ok {
			return nil, moerr.NewInternalError(c.ctx, "no data receiver for sink node")
		}
		ss, err := c.compilePlanScope(ctx, step, n.Children[0], ns)
		if err != nil {
			return nil, err
		}
		rs := c.newMergeScope(ss)
		rs.appendInstruction(vm.Instruction{
			Op:  vm.Dispatch,
			Arg: constructDispatchLocal(true, receivers),
		})

		return []*Scope{rs}, nil
	default:
		return nil, moerr.NewNYI(ctx, fmt.Sprintf("query '%s'", n))
	}
}

func (c *Compile) appendStepRegs(step int32, reg *process.WaitRegister) {
	if _, ok := c.stepRegs[step]; !ok {
		c.stepRegs[step] = make([]*process.WaitRegister, 0, 1)
	}
	c.stepRegs[step] = append(c.stepRegs[step], reg)
}

func (c *Compile) getStepRegs(step int32) ([]*process.WaitRegister, bool) {
	if channels, ok := c.stepRegs[step]; !ok {
		return nil, false
	} else {
		return channels, true
	}
}

func (c *Compile) constructScopeForExternal(addr string, parallel bool) *Scope {
	ds := &Scope{Magic: Normal}
	if parallel {
		ds.Magic = Remote
	}
	ds.NodeInfo = engine.Node{Addr: addr, Mcpu: c.NumCPU()}
	ds.Proc = process.NewWithAnalyze(c.proc, c.ctx, 0, c.anal.Nodes())
	c.proc.LoadTag = c.anal.qry.LoadTag
	ds.Proc.LoadTag = true
	bat := batch.NewWithSize(1)
	{
		bat.Vecs[0] = vector.NewConstNull(types.T_int64.ToType(), 1, c.proc.Mp())
		bat.InitZsOne(1)
	}
	ds.DataSource = &Source{Bat: bat}
	return ds
}

func (c *Compile) constructLoadMergeScope() *Scope {
	ds := &Scope{Magic: Merge}
	ds.Proc = process.NewWithAnalyze(c.proc, c.ctx, 1, c.anal.Nodes())
	ds.Proc.LoadTag = true
	ds.appendInstruction(vm.Instruction{
		Op:      vm.Merge,
		Idx:     c.anal.curr,
		IsFirst: c.anal.isFirst,
		Arg:     &merge.Argument{},
	})
	return ds
}

func (c *Compile) compileExternScan(ctx context.Context, n *plan.Node) ([]*Scope, error) {
	ctx, span := trace.Start(ctx, "compileExternScan")
	defer span.End()
	ID2Addr := make(map[int]int, 0)
	mcpu := 0
	for i := 0; i < len(c.cnList); i++ {
		tmp := mcpu
		mcpu += c.cnList[i].Mcpu
		ID2Addr[i] = mcpu - tmp
	}
	param := &tree.ExternParam{}
	err := json.Unmarshal([]byte(n.TableDef.Createsql), param)
	if err != nil {
		return nil, err
	}
	if param.ScanType == tree.S3 {
		if err := plan2.InitS3Param(param); err != nil {
			return nil, err
		}
		if param.Parallel {
			mcpu = 0
			ID2Addr = make(map[int]int, 0)
			for i := 0; i < len(c.cnList); i++ {
				tmp := mcpu
				if c.cnList[i].Mcpu > external.S3_PARALLEL_MAXNUM {
					mcpu += external.S3_PARALLEL_MAXNUM
				} else {
					mcpu += c.cnList[i].Mcpu
				}
				ID2Addr[i] = mcpu - tmp
			}
		}
	} else {
		if err := plan2.InitInfileParam(param); err != nil {
			return nil, err
		}
	}

	param.FileService = c.proc.FileService
	param.Ctx = c.ctx
	var fileList []string
	var fileSize []int64
	if !param.Local {
		if param.QueryResult {
			fileList = strings.Split(param.Filepath, ",")
			for i := range fileList {
				fileList[i] = strings.TrimSpace(fileList[i])
			}
		} else {
			_, spanReadDir := trace.Start(ctx, "compileExternScan.ReadDir")
			fileList, fileSize, err = plan2.ReadDir(param)
			if err != nil {
				spanReadDir.End()
				return nil, err
			}
			spanReadDir.End()
		}
		fileList, fileSize, err = external.FilterFileList(ctx, n, c.proc, fileList, fileSize)
		if err != nil {
			return nil, err
		}
		if param.LoadFile && len(fileList) == 0 {
			return nil, moerr.NewInvalidInput(ctx, "the file does not exist in load flow")
		}
	} else {
		fileList = []string{param.Filepath}
	}

	if len(fileList) == 0 {
		ret := &Scope{
			Magic:      Normal,
			DataSource: nil,
			Proc:       process.NewWithAnalyze(c.proc, c.ctx, 0, c.anal.Nodes()),
		}

		return []*Scope{ret}, nil
	}

	if param.Parallel && (external.GetCompressType(param, fileList[0]) != tree.NOCOMPRESS || param.Local) {
		return c.compileExternScanParallel(n, param, fileList, fileSize, ctx)
	}

	var fileOffset [][]int64
	for i := 0; i < len(fileList); i++ {
		param.Filepath = fileList[i]
		if param.Parallel {
			arr, err := external.ReadFileOffset(param, c.proc, mcpu, fileSize[i])
			fileOffset = append(fileOffset, arr)
			if err != nil {
				return nil, err
			}
		}
	}
	ss := make([]*Scope, 1)
	if param.Parallel {
		ss = make([]*Scope, len(c.cnList))
	}
	pre := 0
	for i := range ss {
		ss[i] = c.constructScopeForExternal(c.cnList[i].Addr, param.Parallel)
		ss[i].IsLoad = true
		count := ID2Addr[i]
		fileOffsetTmp := make([]*pipeline.FileOffset, len(fileList))
		for j := range fileOffsetTmp {
			preIndex := pre
			fileOffsetTmp[j] = &pipeline.FileOffset{}
			fileOffsetTmp[j].Offset = make([]int64, 0)
			if param.Parallel {
				fileOffsetTmp[j].Offset = append(fileOffsetTmp[j].Offset, fileOffset[j][2*preIndex:2*preIndex+2*count]...)
			} else {
				fileOffsetTmp[j].Offset = append(fileOffsetTmp[j].Offset, []int64{0, -1}...)
			}
		}
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.External,
			Idx:     c.anal.curr,
			IsFirst: c.anal.isFirst,
			Arg:     constructExternal(n, param, c.ctx, fileList, fileSize, fileOffsetTmp),
		})
		pre += count
	}

	return ss, nil
}

// construct one thread to read the file data, then dispatch to mcpu thread to get the filedata for insert
func (c *Compile) compileExternScanParallel(n *plan.Node, param *tree.ExternParam, fileList []string, fileSize []int64, ctx context.Context) ([]*Scope, error) {
	param.Parallel = false
	mcpu := c.cnList[0].Mcpu
	ss := make([]*Scope, mcpu)
	for i := 0; i < mcpu; i++ {
		ss[i] = c.constructLoadMergeScope()
	}
	fileOffsetTmp := make([]*pipeline.FileOffset, len(fileList))
	for i := 0; i < len(fileList); i++ {
		fileOffsetTmp[i] = &pipeline.FileOffset{}
		fileOffsetTmp[i].Offset = make([]int64, 0)
		fileOffsetTmp[i].Offset = append(fileOffsetTmp[i].Offset, []int64{0, -1}...)
	}
	extern := constructExternal(n, param, c.ctx, fileList, fileSize, fileOffsetTmp)
	extern.Es.ParallelLoad = true
	scope := c.constructScopeForExternal("", false)
	scope.appendInstruction(vm.Instruction{
		Op:      vm.External,
		Idx:     c.anal.curr,
		IsFirst: c.anal.isFirst,
		Arg:     extern,
	})
	_, arg := constructDispatchLocalAndRemote(0, ss, c.addr)
	arg.FuncId = dispatch.SendToAnyLocalFunc
	scope.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: arg,
	})
	ss[0].PreScopes = append(ss[0].PreScopes, scope)
	c.anal.isFirst = false
	return ss, nil
}

func (c *Compile) compileTableFunction(n *plan.Node, ss []*Scope) []*Scope {
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.TableFunction,
			Idx:     c.anal.curr,
			IsFirst: currentFirstFlag,
			Arg:     constructTableFunction(n),
		})
	}
	c.anal.isFirst = false

	return ss
}

func (c *Compile) compileTableScan(n *plan.Node) ([]*Scope, error) {
	nodes, err := c.generateNodes(n)
	if err != nil {
		return nil, err
	}
	ss := make([]*Scope, 0, len(nodes))
	for i := range nodes {
		ss = append(ss, c.compileTableScanWithNode(n, nodes[i]))
	}
	return ss, nil
}

func (c *Compile) compileTableScanWithNode(n *plan.Node, node engine.Node) *Scope {
	var err error
	var s *Scope
	var tblDef *plan.TableDef
	var ts timestamp.Timestamp
	var db engine.Database
	var rel engine.Relation

	attrs := make([]string, len(n.TableDef.Cols))
	for j, col := range n.TableDef.Cols {
		attrs[j] = col.Name
	}
	if c.proc != nil && c.proc.TxnOperator != nil {
		ts = c.proc.TxnOperator.Txn().SnapshotTS
	}
	{
		var cols []*plan.ColDef
		ctx := c.ctx
		if util.TableIsClusterTable(n.TableDef.GetTableType()) {
			ctx = context.WithValue(ctx, defines.TenantIDKey{}, catalog.System_Account)
		}
		if n.ObjRef.PubAccountId != -1 {
			ctx = context.WithValue(ctx, defines.TenantIDKey{}, uint32(n.ObjRef.PubAccountId))
		}
		db, err = c.e.Database(ctx, n.ObjRef.SchemaName, c.proc.TxnOperator)
		if err != nil {
			panic(err)
		}
		rel, err = db.Relation(ctx, n.TableDef.Name)
		if err != nil {
			var e error // avoid contamination of error messages
			db, e = c.e.Database(c.ctx, defines.TEMPORARY_DBNAME, c.proc.TxnOperator)
			if e != nil {
				panic(e)
			}
			rel, e = db.Relation(c.ctx, engine.GetTempTableName(n.ObjRef.SchemaName, n.TableDef.Name))
			if e != nil {
				panic(e)
			}
		}
		// defs has no rowid
		defs, err := rel.TableDefs(ctx)
		if err != nil {
			panic(err)
		}
		i := int32(0)
		name2index := make(map[string]int32)
		for _, def := range defs {
			if attr, ok := def.(*engine.AttributeDef); ok {
				name2index[attr.Attr.Name] = i
				cols = append(cols, &plan.ColDef{
					ColId: attr.Attr.ID,
					Name:  attr.Attr.Name,
					Typ: &plan.Type{
						Id:       int32(attr.Attr.Type.Oid),
						Width:    attr.Attr.Type.Width,
						Scale:    attr.Attr.Type.Scale,
						AutoIncr: attr.Attr.AutoIncrement,
					},
					Primary:   attr.Attr.Primary,
					Default:   attr.Attr.Default,
					OnUpdate:  attr.Attr.OnUpdate,
					Comment:   attr.Attr.Comment,
					ClusterBy: attr.Attr.ClusterBy,
					Seqnum:    uint32(attr.Attr.Seqnum),
				})
				i++
			}
		}
		tblDef = &plan.TableDef{
			Cols:          cols,
			Name2ColIndex: name2index,
			Version:       n.TableDef.Version,
			Name:          n.TableDef.Name,
			TableType:     n.TableDef.GetTableType(),
		}
	}

	// prcoess partitioned table
	var partitionRelNames []string
	if n.TableDef.Partition != nil {
		partitionRelNames = append(partitionRelNames, n.TableDef.Partition.PartitionTableNames...)
	}

	s = &Scope{
		Magic:    Remote,
		NodeInfo: node,
		DataSource: &Source{
			Timestamp:              ts,
			Attributes:             attrs,
			TableDef:               tblDef,
			RelationName:           n.TableDef.Name,
			PartitionRelationNames: partitionRelNames,
			SchemaName:             n.ObjRef.SchemaName,
			AccountId:              n.ObjRef.PubAccountId,
			Expr:                   colexec.RewriteFilterExprList(n.FilterList),
		},
	}
	s.Proc = process.NewWithAnalyze(c.proc, c.ctx, 0, c.anal.Nodes())
	return s
}

func (c *Compile) compileRestrict(n *plan.Node, ss []*Scope) []*Scope {
	if len(n.FilterList) == 0 {
		return ss
	}
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.Restrict,
			Idx:     c.anal.curr,
			IsFirst: currentFirstFlag,
			Arg:     constructRestrict(n),
		})
	}
	c.anal.isFirst = false
	return ss
}

func (c *Compile) compileProjection(n *plan.Node, ss []*Scope) []*Scope {
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.Projection,
			Idx:     c.anal.curr,
			IsFirst: currentFirstFlag,
			Arg:     constructProjection(n),
		})
	}
	c.anal.isFirst = false
	return ss
}

func (c *Compile) compileUnion(n *plan.Node, ss []*Scope, children []*Scope) []*Scope {
	ss = append(ss, children...)
	rs := c.newScopeList(1, int(n.Stats.BlockNum))
	gn := new(plan.Node)
	gn.GroupBy = make([]*plan.Expr, len(n.ProjectList))
	for i := range gn.GroupBy {
		gn.GroupBy[i] = plan2.DeepCopyExpr(n.ProjectList[i])
		gn.GroupBy[i].Typ.NotNullable = false
	}
	idx := 0
	for i := range rs {
		rs[i].Instructions = append(rs[i].Instructions, vm.Instruction{
			Op:  vm.Group,
			Idx: c.anal.curr,
			Arg: constructGroup(c.ctx, gn, n, i, len(rs), true, c.proc),
		})
		if isSameCN(rs[i].NodeInfo.Addr, c.addr) {
			idx = i
		}
	}
	mergeChildren := c.newMergeScope(ss)
	mergeChildren.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: constructBroadcastDispatch(0, rs, c.addr, false, 0),
	})
	rs[idx].PreScopes = append(rs[idx].PreScopes, mergeChildren)
	return rs
}

func (c *Compile) compileMinusAndIntersect(n *plan.Node, ss []*Scope, children []*Scope, nodeType plan.Node_NodeType) []*Scope {
	rs := c.newJoinScopeListWithBucket(c.newScopeList(2, int(n.Stats.BlockNum)), ss, children)
	switch nodeType {
	case plan.Node_MINUS:
		for i := range rs {
			rs[i].Instructions[0] = vm.Instruction{
				Op:  vm.Minus,
				Idx: c.anal.curr,
				Arg: constructMinus(i, len(rs)),
			}
		}
	case plan.Node_INTERSECT:
		for i := range rs {
			rs[i].Instructions[0] = vm.Instruction{
				Op:  vm.Intersect,
				Idx: c.anal.curr,
				Arg: constructIntersect(i, len(rs)),
			}
		}
	case plan.Node_INTERSECT_ALL:
		for i := range rs {
			rs[i].Instructions[0] = vm.Instruction{
				Op:  vm.IntersectAll,
				Idx: c.anal.curr,
				Arg: constructIntersectAll(i, len(rs)),
			}
		}
	}
	return rs
}

func (c *Compile) compileUnionAll(ss []*Scope, children []*Scope) []*Scope {
	rs := c.newMergeScope(append(ss, children...))
	rs.Instructions[0].Idx = c.anal.curr
	return []*Scope{rs}
}

func (c *Compile) compileJoin(ctx context.Context, node, left, right *plan.Node, ss []*Scope, children []*Scope) []*Scope {
	var rs []*Scope
	isEq := plan2.IsEquiJoin(node.OnList)

	rightTyps := make([]types.Type, len(right.ProjectList))
	for i, expr := range right.ProjectList {
		rightTyps[i] = dupType(expr.Typ)
	}

	leftTyps := make([]types.Type, len(left.ProjectList))
	for i, expr := range left.ProjectList {
		leftTyps[i] = dupType(expr.Typ)
	}

	switch node.JoinType {
	case plan.Node_INNER:
		rs = c.newBroadcastJoinScopeList(ss, children)
		if len(node.OnList) == 0 {
			for i := range rs {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.Product,
					Idx: c.anal.curr,
					Arg: constructProduct(node, rightTyps, c.proc),
				})
			}
		} else {
			for i := range rs {
				if isEq {
					rs[i].appendInstruction(vm.Instruction{
						Op:  vm.Join,
						Idx: c.anal.curr,
						Arg: constructJoin(node, rightTyps, c.proc),
					})
				} else {
					rs[i].appendInstruction(vm.Instruction{
						Op:  vm.LoopJoin,
						Idx: c.anal.curr,
						Arg: constructLoopJoin(node, rightTyps, c.proc),
					})
				}
			}
		}
	case plan.Node_SEMI:
		if isEq {
			if node.BuildOnLeft {
				rs = c.newJoinScopeListWithBucket(c.newScopeListForRightJoin(2, ss), ss, children)
				for i := range rs {
					rs[i].appendInstruction(vm.Instruction{
						Op:  vm.RightSemi,
						Idx: c.anal.curr,
						Arg: constructRightSemi(node, rightTyps, uint64(i), uint64(len(rs)), c.proc),
					})
				}
			} else {
				rs = c.newBroadcastJoinScopeList(ss, children)
				for i := range rs {
					rs[i].appendInstruction(vm.Instruction{
						Op:  vm.Semi,
						Idx: c.anal.curr,
						Arg: constructSemi(node, rightTyps, c.proc),
					})
				}
			}
		} else {
			rs = c.newBroadcastJoinScopeList(ss, children)
			for i := range rs {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.LoopSemi,
					Idx: c.anal.curr,
					Arg: constructLoopSemi(node, rightTyps, c.proc),
				})
			}
		}
	case plan.Node_LEFT:
		rs = c.newBroadcastJoinScopeList(ss, children)
		for i := range rs {
			if isEq {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.Left,
					Idx: c.anal.curr,
					Arg: constructLeft(node, rightTyps, c.proc),
				})
			} else {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.LoopLeft,
					Idx: c.anal.curr,
					Arg: constructLoopLeft(node, rightTyps, c.proc),
				})
			}
		}
	case plan.Node_RIGHT:
		if isEq {
			rs = c.newJoinScopeListWithBucket(c.newScopeListForRightJoin(2, ss), ss, children)
			for i := range rs {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.Right,
					Idx: c.anal.curr,
					Arg: constructRight(node, leftTyps, rightTyps, uint64(i), uint64(len(rs)), c.proc),
				})
			}
		} else {
			panic("dont pass any no-equal right join plan to this function,it should be changed to left join by the planner")
		}
	case plan.Node_SINGLE:
		rs = c.newBroadcastJoinScopeList(ss, children)
		for i := range rs {
			if isEq {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.Single,
					Idx: c.anal.curr,
					Arg: constructSingle(node, rightTyps, c.proc),
				})
			} else {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.LoopSingle,
					Idx: c.anal.curr,
					Arg: constructLoopSingle(node, c.proc),
				})
			}
		}
	case plan.Node_ANTI:
		if isEq {
			if node.BuildOnLeft {
				rs = c.newJoinScopeListWithBucket(c.newScopeListForRightJoin(2, ss), ss, children)
				for i := range rs {
					rs[i].appendInstruction(vm.Instruction{
						Op:  vm.RightAnti,
						Idx: c.anal.curr,
						Arg: constructRightAnti(node, rightTyps, uint64(i), uint64(len(rs)), c.proc),
					})
				}
			} else {
				rs = c.newBroadcastJoinScopeList(ss, children)
				for i := range rs {
					rs[i].appendInstruction(vm.Instruction{
						Op:  vm.Anti,
						Idx: c.anal.curr,
						Arg: constructAnti(node, rightTyps, c.proc),
					})
				}
			}
		} else {
			rs = c.newBroadcastJoinScopeList(ss, children)
			for i := range rs {
				rs[i].appendInstruction(vm.Instruction{
					Op:  vm.LoopAnti,
					Idx: c.anal.curr,
					Arg: constructLoopAnti(node, rightTyps, c.proc),
				})
			}
		}
	case plan.Node_MARK:
		rs = c.newBroadcastJoinScopeList(ss, children)
		for i := range rs {
			//if isEq {
			//	rs[i].appendInstruction(vm.Instruction{
			//		Op:  vm.Mark,
			//		Idx: c.anal.curr,
			//		Arg: constructMark(n, typs, c.proc),
			//	})
			//} else {
			rs[i].appendInstruction(vm.Instruction{
				Op:  vm.LoopMark,
				Idx: c.anal.curr,
				Arg: constructLoopMark(node, rightTyps, c.proc),
			})
			//}
		}
	default:
		panic(moerr.NewNYI(ctx, fmt.Sprintf("join typ '%v'", node.JoinType)))
	}
	return rs
}

func (c *Compile) compileSort(n *plan.Node, ss []*Scope) []*Scope {
	switch {
	case n.Limit != nil && n.Offset == nil && len(n.OrderBy) > 0: // top
		vec, err := colexec.EvalExpressionOnce(c.proc, n.Limit, []*batch.Batch{constBat})
		if err != nil {
			panic(err)
		}
		defer vec.Free(c.proc.Mp())
		return c.compileTop(n, vector.MustFixedCol[int64](vec)[0], ss)

	case n.Limit == nil && n.Offset == nil && len(n.OrderBy) > 0: // top
		return c.compileOrder(n, ss)

	case n.Limit != nil && n.Offset != nil && len(n.OrderBy) > 0:
		// get limit
		vec1, err := colexec.EvalExpressionOnce(c.proc, n.Limit, []*batch.Batch{constBat})
		if err != nil {
			panic(err)
		}
		defer vec1.Free(c.proc.Mp())

		// get offset
		vec2, err := colexec.EvalExpressionOnce(c.proc, n.Offset, []*batch.Batch{constBat})
		if err != nil {
			panic(err)
		}
		defer vec2.Free(c.proc.Mp())

		limit, offset := vector.MustFixedCol[int64](vec1)[0], vector.MustFixedCol[int64](vec2)[0]
		topN := limit + offset
		if topN <= 8192*2 {
			// if n is small, convert `order by col limit m offset n` to `top m+n offset n`
			return c.compileOffset(n, c.compileTop(n, topN, ss))
		}
		return c.compileLimit(n, c.compileOffset(n, c.compileOrder(n, ss)))

	case n.Limit == nil && n.Offset != nil && len(n.OrderBy) > 0: // order and offset
		return c.compileOffset(n, c.compileOrder(n, ss))

	case n.Limit != nil && n.Offset == nil && len(n.OrderBy) == 0: // limit
		return c.compileLimit(n, ss)

	case n.Limit == nil && n.Offset != nil && len(n.OrderBy) == 0: // offset
		return c.compileOffset(n, ss)

	case n.Limit != nil && n.Offset != nil && len(n.OrderBy) == 0: // limit and offset
		return c.compileLimit(n, c.compileOffset(n, ss))

	default:
		return ss
	}
}

func containBrokenNode(s *Scope) bool {
	for i := range s.Instructions {
		if s.Instructions[i].IsBrokenNode() {
			return true
		}
	}
	return false
}

func (c *Compile) compileTop(n *plan.Node, topN int64, ss []*Scope) []*Scope {
	// use topN TO make scope.
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		c.anal.isFirst = currentFirstFlag
		if containBrokenNode(ss[i]) {
			ss[i] = c.newMergeScope([]*Scope{ss[i]})
		}
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.Top,
			Idx:     c.anal.curr,
			IsFirst: c.anal.isFirst,
			Arg:     constructTop(n, topN),
		})
	}
	c.anal.isFirst = false

	rs := c.newMergeScope(ss)
	rs.Instructions[0] = vm.Instruction{
		Op:  vm.MergeTop,
		Idx: c.anal.curr,
		Arg: constructMergeTop(n, topN),
	}
	return []*Scope{rs}
}

func (c *Compile) compileOrder(n *plan.Node, ss []*Scope) []*Scope {
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		c.anal.isFirst = currentFirstFlag
		if containBrokenNode(ss[i]) {
			ss[i] = c.newMergeScope([]*Scope{ss[i]})
		}
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.Order,
			Idx:     c.anal.curr,
			IsFirst: c.anal.isFirst,
			Arg:     constructOrder(n),
		})
	}
	c.anal.isFirst = false

	rs := c.newMergeScope(ss)
	rs.Instructions[0] = vm.Instruction{
		Op:  vm.MergeOrder,
		Idx: c.anal.curr,
		Arg: constructMergeOrder(n),
	}
	return []*Scope{rs}
}

func (c *Compile) compileOffset(n *plan.Node, ss []*Scope) []*Scope {
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		if containBrokenNode(ss[i]) {
			c.anal.isFirst = currentFirstFlag
			ss[i] = c.newMergeScope([]*Scope{ss[i]})
		}
	}

	rs := c.newMergeScope(ss)
	rs.Instructions[0] = vm.Instruction{
		Op:  vm.MergeOffset,
		Idx: c.anal.curr,
		Arg: constructMergeOffset(n, c.proc),
	}
	return []*Scope{rs}
}

func (c *Compile) compileLimit(n *plan.Node, ss []*Scope) []*Scope {
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		c.anal.isFirst = currentFirstFlag
		if containBrokenNode(ss[i]) {
			ss[i] = c.newMergeScope([]*Scope{ss[i]})
		}
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.Limit,
			Idx:     c.anal.curr,
			IsFirst: c.anal.isFirst,
			Arg:     constructLimit(n, c.proc),
		})
	}
	c.anal.isFirst = false

	rs := c.newMergeScope(ss)
	rs.Instructions[0] = vm.Instruction{
		Op:  vm.MergeLimit,
		Idx: c.anal.curr,
		Arg: constructMergeLimit(n, c.proc),
	}
	return []*Scope{rs}
}

func (c *Compile) compileMergeGroup(n *plan.Node, ss []*Scope, ns []*plan.Node) []*Scope {
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		c.anal.isFirst = currentFirstFlag
		if containBrokenNode(ss[i]) {
			ss[i] = c.newMergeScope([]*Scope{ss[i]})
		}
		ss[i].appendInstruction(vm.Instruction{
			Op:      vm.Group,
			Idx:     c.anal.curr,
			IsFirst: c.anal.isFirst,
			Arg:     constructGroup(c.ctx, n, ns[n.Children[0]], 0, 0, false, c.proc),
		})
	}
	c.anal.isFirst = false

	rs := c.newMergeScope(ss)
	rs.Instructions[0] = vm.Instruction{
		Op:  vm.MergeGroup,
		Idx: c.anal.curr,
		Arg: constructMergeGroup(true),
	}
	return []*Scope{rs}
}

func (c *Compile) compileBucketGroup(n *plan.Node, ss []*Scope, ns []*plan.Node, idxToShuffle int) []*Scope {
	currentIsFirst := c.anal.isFirst
	c.anal.isFirst = false
	dop := plan2.GetShuffleDop()
	parent, children := c.newScopeListForGroup(validScopeCount(ss), dop)

	j := 0
	hashColumnIdx := plan2.GetHashColumnIdx(n.GroupBy[idxToShuffle])
	for i := range ss {
		if containBrokenNode(ss[i]) {
			isEnd := ss[i].IsEnd
			ss[i] = c.newMergeScope([]*Scope{ss[i]})
			ss[i].IsEnd = isEnd
		}
		if !ss[i].IsEnd {
			ss[i].appendInstruction(vm.Instruction{
				Op:  vm.Dispatch,
				Arg: constructBroadcastDispatch(j, children, ss[i].NodeInfo.Addr, true, hashColumnIdx),
			})
			j++
			ss[i].IsEnd = true
		}
	}

	// saving the last operator of all children to make sure the connector setting in
	// the right place
	lastOperator := make([]vm.Instruction, 0, len(children))
	for i := range children {
		ilen := len(children[i].Instructions) - 1
		lastOperator = append(lastOperator, children[i].Instructions[ilen])
		children[i].Instructions = children[i].Instructions[:ilen]
	}

	for i := range children {
		children[i].Instructions = append(children[i].Instructions, vm.Instruction{
			Op:      vm.Group,
			Idx:     c.anal.curr,
			IsFirst: currentIsFirst,
			Arg:     constructGroup(c.ctx, n, ns[n.Children[0]], 0, 0, true, c.proc),
		})
	}

	children = c.compileProjection(n, children)

	// recovery the children's last operator
	for i := range children {
		children[i].Instructions = append(children[i].Instructions, lastOperator[i])
	}

	for i := range ss {
		appended := false
		for j := range children {
			if children[j].NodeInfo.Addr == ss[i].NodeInfo.Addr {
				children[j].PreScopes = append(children[j].PreScopes, ss[i])
				appended = true
				break
			}
		}
		if !appended {
			children[0].PreScopes = append(children[0].PreScopes, ss[i])
		}
	}

	return []*Scope{c.newMergeScope(parent)}
}

func (c *Compile) newInsertMergeScope(arg *insert.Argument, ss []*Scope) *Scope {
	ss2 := make([]*Scope, 0, len(ss))
	for _, s := range ss {
		if s.IsEnd {
			continue
		}
		ss2 = append(ss2, s)
	}
	insert := &vm.Instruction{
		Op:  vm.Insert,
		Arg: arg,
	}
	for i := range ss2 {
		ss2[i].Instructions = append(ss2[i].Instructions, dupInstruction(insert, nil, i))
	}
	return c.newMergeScope(ss2)
}

// DeleteMergeScope need to assure this:
// one block can be only deleted by one and the same
// CN, so we need to transfer the rows from the
// the same block to one and the same CN to perform
// the deletion operators.
func (c *Compile) newDeleteMergeScope(arg *deletion.Argument, ss []*Scope) *Scope {
	//Todo: implemet delete merge
	ss2 := make([]*Scope, 0, len(ss))
	// ends := make([]*Scope, 0, len(ss))
	for _, s := range ss {
		if s.IsEnd {
			// ends = append(ends, s)
			continue
		}
		ss2 = append(ss2, s)
	}

	rs := make([]*Scope, 0, len(ss2))
	uuids := make([]uuid.UUID, 0, len(ss2))
	for i := 0; i < len(ss2); i++ {
		rs = append(rs, new(Scope))
		uuids = append(uuids, uuid.New())
	}

	// for every scope, it should dispatch its
	// batch to other cn
	for i := 0; i < len(ss2); i++ {
		constructDeleteDispatchAndLocal(i, rs, ss2, uuids, c)
	}
	delete := &vm.Instruction{
		Op:  vm.Deletion,
		Arg: arg,
	}
	for i := range rs {
		// use distributed delete
		arg.RemoteDelete = true
		// maybe just copy only once?
		arg.SegmentMap = colexec.Srv.GetCnSegmentMap()
		arg.IBucket = uint32(i)
		arg.Nbucket = uint32(len(rs))
		rs[i].Instructions = append(rs[i].Instructions, dupInstruction(delete, nil, 0))
	}
	return c.newMergeScope(rs)
}

func (c *Compile) newMergeScope(ss []*Scope) *Scope {
	rs := &Scope{
		PreScopes: ss,
		Magic:     Merge,
		NodeInfo: engine.Node{
			Addr: c.addr,
			Mcpu: c.NumCPU(),
		},
	}
	cnt := 0
	for _, s := range ss {
		if s.IsEnd {
			continue
		}
		cnt++
	}
	rs.Proc = process.NewWithAnalyze(c.proc, c.ctx, cnt, c.anal.Nodes())
	if len(ss) > 0 {
		rs.Proc.LoadTag = ss[0].Proc.LoadTag
	}
	rs.Instructions = append(rs.Instructions, vm.Instruction{
		Op:      vm.Merge,
		Idx:     c.anal.curr,
		IsFirst: c.anal.isFirst,
		Arg:     &merge.Argument{},
	})
	c.anal.isFirst = false

	j := 0
	for i := range ss {
		if !ss[i].IsEnd {
			ss[i].appendInstruction(vm.Instruction{
				Op: vm.Connector,
				Arg: &connector.Argument{
					Reg: rs.Proc.Reg.MergeReceivers[j],
				},
			})
			j++
		}
	}
	return rs
}

func (c *Compile) newMergeRemoteScope(ss []*Scope, nodeinfo engine.Node) *Scope {
	rs := c.newMergeScope(ss)
	// reset rs's info to remote
	rs.Magic = Remote
	rs.NodeInfo.Addr = nodeinfo.Addr
	rs.NodeInfo.Mcpu = nodeinfo.Mcpu

	return rs
}

func (c *Compile) newScopeList(childrenCount int, blocks int) []*Scope {
	var ss []*Scope

	currentFirstFlag := c.anal.isFirst
	for _, n := range c.cnList {
		c.anal.isFirst = currentFirstFlag
		ss = append(ss, c.newScopeListWithNode(c.generateCPUNumber(n.Mcpu, blocks), childrenCount, n.Addr)...)
	}
	return ss
}

func (c *Compile) newScopeListForGroup(childrenCount int, blocks int) ([]*Scope, []*Scope) {
	var parent = make([]*Scope, 0, len(c.cnList))
	var children = make([]*Scope, 0, len(c.cnList))

	currentFirstFlag := c.anal.isFirst
	for _, n := range c.cnList {
		c.anal.isFirst = currentFirstFlag
		scopes := c.newScopeListWithNode(c.generateCPUNumber(n.Mcpu, blocks), childrenCount, n.Addr)
		children = append(children, scopes...)
		parent = append(parent, c.newMergeRemoteScope(scopes, n))
	}
	return parent, children
}

func (c *Compile) newScopeListWithNode(mcpu, childrenCount int, addr string) []*Scope {
	ss := make([]*Scope, mcpu)
	currentFirstFlag := c.anal.isFirst
	for i := range ss {
		ss[i] = new(Scope)
		ss[i].Magic = Remote
		ss[i].NodeInfo.Addr = addr
		ss[i].NodeInfo.Mcpu = 1 // ss is already the mcpu length so we don't need to parallel it
		ss[i].Proc = process.NewWithAnalyze(c.proc, c.ctx, childrenCount, c.anal.Nodes())
		ss[i].Instructions = append(ss[i].Instructions, vm.Instruction{
			Op:      vm.Merge,
			Idx:     c.anal.curr,
			IsFirst: currentFirstFlag,
			Arg:     &merge.Argument{},
		})
	}
	c.anal.isFirst = false
	return ss
}

func (c *Compile) newScopeListForRightJoin(childrenCount int, leftScopes []*Scope) []*Scope {
	/*
		ss := make([]*Scope, 0, len(leftScopes))
		for i := range leftScopes {
			tmp := new(Scope)
			tmp.Magic = Remote
			tmp.IsJoin = true
			tmp.Proc = process.NewWithAnalyze(c.proc, c.ctx, childrenCount, c.anal.Nodes())
			tmp.NodeInfo = leftScopes[i].NodeInfo
			ss = append(ss, tmp)
		}
	*/

	// Force right join to execute on one CN due to right join issue
	// Will fix in future
	maxCpuNum := 1
	for _, s := range leftScopes {
		if s.NodeInfo.Mcpu > maxCpuNum {
			maxCpuNum = s.NodeInfo.Mcpu
		}
	}

	ss := make([]*Scope, 1)
	ss[0] = &Scope{
		Magic:    Remote,
		IsJoin:   true,
		Proc:     process.NewWithAnalyze(c.proc, c.ctx, childrenCount, c.anal.Nodes()),
		NodeInfo: engine.Node{Addr: c.addr, Mcpu: c.generateCPUNumber(c.NumCPU(), maxCpuNum)},
	}
	return ss
}

func (c *Compile) newJoinScopeListWithBucket(rs, ss, children []*Scope) []*Scope {
	currentFirstFlag := c.anal.isFirst
	// construct left
	leftMerge := c.newMergeScope(ss)
	leftMerge.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: constructBroadcastDispatch(0, rs, c.addr, false, 0),
	})
	leftMerge.IsEnd = true

	// construct right
	c.anal.isFirst = currentFirstFlag
	rightMerge := c.newMergeScope(children)
	rightMerge.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: constructBroadcastDispatch(1, rs, c.addr, false, 0),
	})
	rightMerge.IsEnd = true

	// append left and right to correspond rs
	idx := 0
	for i := range rs {
		if isSameCN(rs[i].NodeInfo.Addr, c.addr) {
			idx = i
		}
	}
	rs[idx].PreScopes = append(rs[idx].PreScopes, leftMerge, rightMerge)
	return rs
}

//func (c *Compile) newJoinScopeList(ss []*Scope, children []*Scope) []*Scope {
//rs := make([]*Scope, len(ss))
//// join's input will record in the left/right scope when JoinRun
//// so set it to false here.
//c.anal.isFirst = false
//for i := range ss {
//if ss[i].IsEnd {
//rs[i] = ss[i]
//continue
//}
//chp := c.newMergeScope(dupScopeList(children))
//rs[i] = new(Scope)
//rs[i].Magic = Remote
//rs[i].IsJoin = true
//rs[i].NodeInfo = ss[i].NodeInfo
//rs[i].PreScopes = []*Scope{ss[i], chp}
//rs[i].Proc = process.NewWithAnalyze(c.proc, c.ctx, 2, c.anal.Nodes())
//ss[i].appendInstruction(vm.Instruction{
//Op: vm.Connector,
//Arg: &connector.Argument{
//Reg: rs[i].Proc.Reg.MergeReceivers[0],
//},
//})
//chp.appendInstruction(vm.Instruction{
//Op: vm.Connector,
//Arg: &connector.Argument{
//Reg: rs[i].Proc.Reg.MergeReceivers[1],
//},
//})
//chp.IsEnd = true
//}
//return rs
//}

func (c *Compile) newBroadcastJoinScopeList(ss []*Scope, children []*Scope) []*Scope {
	length := len(ss)
	rs := make([]*Scope, length)
	idx := 0
	for i := range ss {
		if ss[i].IsEnd {
			rs[i] = ss[i]
			continue
		}
		rs[i] = new(Scope)
		rs[i].Magic = Remote
		rs[i].IsJoin = true
		rs[i].NodeInfo = ss[i].NodeInfo
		if isSameCN(rs[i].NodeInfo.Addr, c.addr) {
			idx = i
		}
		rs[i].PreScopes = []*Scope{ss[i]}
		rs[i].Proc = process.NewWithAnalyze(c.proc, c.ctx, 2, c.anal.Nodes())
		ss[i].appendInstruction(vm.Instruction{
			Op: vm.Connector,
			Arg: &connector.Argument{
				Reg: rs[i].Proc.Reg.MergeReceivers[0],
			},
		})
	}

	// all join's first flag will setting in newLeftScope and newRightScope
	// so we set it to false now
	c.anal.isFirst = false
	mergeChildren := c.newMergeScope(children)
	mergeChildren.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: constructBroadcastDispatch(1, rs, c.addr, false, 0),
	})
	mergeChildren.IsEnd = true
	rs[idx].PreScopes = append(rs[idx].PreScopes, mergeChildren)

	return rs
}

func (c *Compile) newJoinProbeScope(s *Scope, ss []*Scope) *Scope {
	rs := &Scope{
		Magic: Merge,
	}
	rs.appendInstruction(vm.Instruction{
		Op:      vm.Merge,
		Idx:     s.Instructions[0].Idx,
		IsFirst: true,
		Arg:     &merge.Argument{},
	})
	rs.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: constructDispatchLocal(false, extraRegisters(ss, 0)),
	})
	rs.IsEnd = true
	rs.Proc = process.NewWithAnalyze(s.Proc, s.Proc.Ctx, 1, c.anal.Nodes())
	regTransplant(s, rs, 0, 0)
	return rs
}

func (c *Compile) newJoinBuildScope(s *Scope, ss []*Scope) *Scope {
	rs := &Scope{
		Magic: Merge,
	}
	rs.appendInstruction(vm.Instruction{
		Op:      vm.HashBuild,
		Idx:     s.Instructions[0].Idx,
		IsFirst: true,
		Arg:     constructHashBuild(s.Instructions[0], c.proc),
	})
	rs.appendInstruction(vm.Instruction{
		Op:  vm.Dispatch,
		Arg: constructDispatchLocal(true, extraRegisters(ss, 1)),
	})
	rs.IsEnd = true
	rs.Proc = process.NewWithAnalyze(s.Proc, s.Proc.Ctx, 1, c.anal.Nodes())
	regTransplant(s, rs, 1, 0)
	return rs
}

// Transplant the source's RemoteReceivRegInfos which index equal to sourceIdx to
// target with new index targetIdx
func regTransplant(source, target *Scope, sourceIdx, targetIdx int) {
	target.Proc.Reg.MergeReceivers[targetIdx] = source.Proc.Reg.MergeReceivers[sourceIdx]
	i := 0
	for i < len(source.RemoteReceivRegInfos) {
		op := &source.RemoteReceivRegInfos[i]
		if op.Idx == sourceIdx {
			target.RemoteReceivRegInfos = append(target.RemoteReceivRegInfos, RemoteReceivRegInfo{
				Idx:      targetIdx,
				Uuid:     op.Uuid,
				FromAddr: op.FromAddr,
			})
			source.RemoteReceivRegInfos = append(source.RemoteReceivRegInfos[:i], source.RemoteReceivRegInfos[i+1:]...)
			continue
		}
		i++
	}
}

// Number of cpu's available on the current machine
func (c *Compile) NumCPU() int {
	return runtime.NumCPU()
}

func (c *Compile) generateCPUNumber(cpunum, blocks int) int {
	if cpunum <= 0 || blocks <= 0 {
		return 1
	}

	if cpunum <= blocks {
		return cpunum
	}
	return blocks
}

func (c *Compile) initAnalyze(qry *plan.Query) {
	anals := make([]*process.AnalyzeInfo, len(qry.Nodes))
	for i := range anals {
		anals[i] = new(process.AnalyzeInfo)
	}
	c.anal = &anaylze{
		qry:       qry,
		analInfos: anals,
		curr:      int(qry.Steps[0]),
	}
	c.proc.AnalInfos = c.anal.analInfos
}

func (c *Compile) fillAnalyzeInfo() {
	// record the number of s3 requests
	c.anal.analInfos[c.anal.curr].S3IOInputCount += c.s3CounterSet.FileService.S3.Put.Load()
	c.anal.analInfos[c.anal.curr].S3IOInputCount += c.s3CounterSet.FileService.S3.List.Load()
	c.anal.analInfos[c.anal.curr].S3IOOutputCount += c.s3CounterSet.FileService.S3.Head.Load()
	c.anal.analInfos[c.anal.curr].S3IOOutputCount += c.s3CounterSet.FileService.S3.Get.Load()
	c.anal.analInfos[c.anal.curr].S3IOOutputCount += c.s3CounterSet.FileService.S3.Delete.Load()
	c.anal.analInfos[c.anal.curr].S3IOOutputCount += c.s3CounterSet.FileService.S3.DeleteMulti.Load()
	for i, anal := range c.anal.analInfos {
		if c.anal.qry.Nodes[i].AnalyzeInfo == nil {
			c.anal.qry.Nodes[i].AnalyzeInfo = new(plan.AnalyzeInfo)
		}
		c.anal.qry.Nodes[i].AnalyzeInfo.InputRows = atomic.LoadInt64(&anal.InputRows)
		c.anal.qry.Nodes[i].AnalyzeInfo.OutputRows = atomic.LoadInt64(&anal.OutputRows)
		c.anal.qry.Nodes[i].AnalyzeInfo.InputSize = atomic.LoadInt64(&anal.InputSize)
		c.anal.qry.Nodes[i].AnalyzeInfo.OutputSize = atomic.LoadInt64(&anal.OutputSize)
		c.anal.qry.Nodes[i].AnalyzeInfo.TimeConsumed = atomic.LoadInt64(&anal.TimeConsumed)
		c.anal.qry.Nodes[i].AnalyzeInfo.MemorySize = atomic.LoadInt64(&anal.MemorySize)
		c.anal.qry.Nodes[i].AnalyzeInfo.WaitTimeConsumed = atomic.LoadInt64(&anal.WaitTimeConsumed)
		c.anal.qry.Nodes[i].AnalyzeInfo.DiskIO = atomic.LoadInt64(&anal.DiskIO)
		c.anal.qry.Nodes[i].AnalyzeInfo.S3IOByte = atomic.LoadInt64(&anal.S3IOByte)
		c.anal.qry.Nodes[i].AnalyzeInfo.S3IOInputCount = atomic.LoadInt64(&anal.S3IOInputCount)
		c.anal.qry.Nodes[i].AnalyzeInfo.S3IOOutputCount = atomic.LoadInt64(&anal.S3IOOutputCount)
		c.anal.qry.Nodes[i].AnalyzeInfo.NetworkIO = atomic.LoadInt64(&anal.NetworkIO)
		c.anal.qry.Nodes[i].AnalyzeInfo.ScanTime = atomic.LoadInt64(&anal.ScanTime)
		c.anal.qry.Nodes[i].AnalyzeInfo.InsertTime = atomic.LoadInt64(&anal.InsertTime)
	}
}

func (c *Compile) generateNodes(n *plan.Node) (engine.Nodes, error) {
	var err error
	var db engine.Database
	var rel engine.Relation
	var ranges [][]byte
	var nodes engine.Nodes
	isPartitionTable := false

	ctx := c.ctx
	if util.TableIsClusterTable(n.TableDef.GetTableType()) {
		ctx = context.WithValue(ctx, defines.TenantIDKey{}, catalog.System_Account)
	}
	if n.ObjRef.PubAccountId != -1 {
		ctx = context.WithValue(ctx, defines.TenantIDKey{}, uint32(n.ObjRef.PubAccountId))
	}
	db, err = c.e.Database(ctx, n.ObjRef.SchemaName, c.proc.TxnOperator)
	if err != nil {
		return nil, err
	}
	rel, err = db.Relation(ctx, n.TableDef.Name)
	if err != nil {
		var e error // avoid contamination of error messages
		db, e = c.e.Database(ctx, defines.TEMPORARY_DBNAME, c.proc.TxnOperator)
		if e != nil {
			return nil, err
		}

		// if temporary table, just scan at local cn.
		rel, e = db.Relation(ctx, engine.GetTempTableName(n.ObjRef.SchemaName, n.TableDef.Name))
		if e != nil {
			return nil, err
		}
		c.cnList = engine.Nodes{
			engine.Node{
				Addr: c.addr,
				Rel:  rel,
				Mcpu: 1,
			},
		}
	}

	ranges, err = rel.Ranges(ctx, n.BlockFilterList...)
	if err != nil {
		return nil, err
	}

	if n.TableDef.Partition != nil {
		isPartitionTable = true
		partitionInfo := n.TableDef.Partition
		partitionNum := int(partitionInfo.PartitionNum)
		partitionTableNames := partitionInfo.PartitionTableNames
		for i := 0; i < partitionNum; i++ {
			partTableName := partitionTableNames[i]
			subrelation, err := db.Relation(ctx, partTableName)
			if err != nil {
				return nil, err
			}
			subranges, err := subrelation.Ranges(ctx, n.BlockFilterList...)
			if err != nil {
				return nil, err
			}
			ranges = append(ranges, subranges[1:]...)
		}
	}

	// some log for finding a bug.
	tblId := rel.GetTableID(ctx)
	expectedLen := len(ranges)
	logutil.Debugf("cn generateNodes, tbl %d ranges is %d", tblId, expectedLen)

	// If ranges == 0, dont know what type of table is this
	if len(ranges) == 0 && n.TableDef.TableType != catalog.SystemOrdinaryRel {
		nodes = make(engine.Nodes, len(c.cnList))
		for i, node := range c.cnList {
			if isPartitionTable {
				nodes[i] = engine.Node{
					Id:   node.Id,
					Addr: node.Addr,
					Mcpu: c.generateCPUNumber(node.Mcpu, int(n.Stats.BlockNum)),
				}
			} else {
				nodes[i] = engine.Node{
					Rel:  rel,
					Id:   node.Id,
					Addr: node.Addr,
					Mcpu: c.generateCPUNumber(node.Mcpu, int(n.Stats.BlockNum)),
				}
			}
		}
		return nodes, nil
	}

	engineType := rel.GetEngineType()
	if isPartitionTable {
		rel = nil
	}
	// for multi cn in launch mode, put all payloads in current CN
	// maybe delete this in the future
	if isLaunchMode(c.cnList) {
		return putBlocksInCurrentCN(c, ranges, rel, n), nil
	}
	// disttae engine , hash s3 objects to fixed CN
	if engineType == engine.Disttae {
		return hashBlocksToFixedCN(c, ranges, rel, n), nil
	}
	// maybe temp table on memengine , just put payloads in average
	return putBlocksInAverage(c, ranges, rel, n), nil
}

func putBlocksInAverage(c *Compile, ranges [][]byte, rel engine.Relation, n *plan.Node) engine.Nodes {
	var nodes engine.Nodes
	step := (len(ranges) + len(c.cnList) - 1) / len(c.cnList)
	for i := 0; i < len(ranges); i += step {
		j := i / step
		if i+step >= len(ranges) {
			if isSameCN(c.cnList[j].Addr, c.addr) {
				if len(nodes) == 0 {
					nodes = append(nodes, engine.Node{
						Addr: c.addr,
						Rel:  rel,
						Mcpu: c.generateCPUNumber(c.NumCPU(), int(n.Stats.BlockNum)),
					})
				}
				nodes[0].Data = append(nodes[0].Data, ranges[i:]...)
			} else {
				nodes = append(nodes, engine.Node{
					Rel:  rel,
					Id:   c.cnList[j].Id,
					Addr: c.cnList[j].Addr,
					Mcpu: c.generateCPUNumber(c.cnList[j].Mcpu, int(n.Stats.BlockNum)),
					Data: ranges[i:],
				})
			}
		} else {
			if isSameCN(c.cnList[j].Addr, c.addr) {
				if len(nodes) == 0 {
					nodes = append(nodes, engine.Node{
						Rel:  rel,
						Addr: c.addr,
						Mcpu: c.generateCPUNumber(c.NumCPU(), int(n.Stats.BlockNum)),
					})
				}
				nodes[0].Data = append(nodes[0].Data, ranges[i:i+step]...)
			} else {
				nodes = append(nodes, engine.Node{
					Rel:  rel,
					Id:   c.cnList[j].Id,
					Addr: c.cnList[j].Addr,
					Mcpu: c.generateCPUNumber(c.cnList[j].Mcpu, int(n.Stats.BlockNum)),
					Data: ranges[i : i+step],
				})
			}
		}
	}
	return nodes
}

func hashBlocksToFixedCN(c *Compile, ranges [][]byte, rel engine.Relation, n *plan.Node) engine.Nodes {
	var nodes engine.Nodes
	//add current CN
	nodes = append(nodes, engine.Node{
		Addr: c.addr,
		Rel:  rel,
		Mcpu: c.generateCPUNumber(c.NumCPU(), int(n.Stats.BlockNum)),
	})
	//add memory table block
	nodes[0].Data = append(nodes[0].Data, ranges[:1]...)
	ranges = ranges[1:]
	// only memory table block
	if len(ranges) == 0 {
		return nodes
	}
	//only one cn
	if len(c.cnList) == 1 {
		nodes[0].Data = append(nodes[0].Data, ranges...)
		return nodes
	}
	//add the rest of CNs in list
	for i := range c.cnList {
		if c.cnList[i].Addr != c.addr {
			nodes = append(nodes, engine.Node{
				Rel:  rel,
				Id:   c.cnList[i].Id,
				Addr: c.cnList[i].Addr,
				Mcpu: c.generateCPUNumber(c.cnList[i].Mcpu, int(n.Stats.BlockNum)),
			})
		}
	}
	// sort by addr to get fixed order of CN list
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Addr < nodes[j].Addr })

	//to maxify locality, put blocks in the same s3 object in the same CN
	lenCN := len(c.cnList)
	for i, blk := range ranges {
		unmarshalledBlockInfo := catalog.DecodeBlockInfo(ranges[i])
		// get timestamp in objName to make sure it is random enough
		objTimeStamp := unmarshalledBlockInfo.MetaLocation().Name()[:7]
		index := plan2.SimpleHashToRange(objTimeStamp, lenCN)
		nodes[index].Data = append(nodes[index].Data, blk)
	}
	minWorkLoad := math.MaxInt32
	maxWorkLoad := 0
	//remove empty node from nodes
	var newNodes engine.Nodes
	for i := range nodes {
		if len(nodes[i].Data) > maxWorkLoad {
			maxWorkLoad = len(nodes[i].Data)
		}
		if len(nodes[i].Data) < minWorkLoad {
			minWorkLoad = len(nodes[i].Data)
		}
		if len(nodes[i].Data) > 0 {
			newNodes = append(newNodes, nodes[i])
		}
	}
	if minWorkLoad*2 < maxWorkLoad {
		logutil.Warnf("workload among CNs not balanced, max %v, min %v", maxWorkLoad, minWorkLoad)
	}
	return newNodes
}

func putBlocksInCurrentCN(c *Compile, ranges [][]byte, rel engine.Relation, n *plan.Node) engine.Nodes {
	var nodes engine.Nodes
	//add current CN
	nodes = append(nodes, engine.Node{
		Addr: c.addr,
		Rel:  rel,
		Mcpu: c.generateCPUNumber(c.NumCPU(), int(n.Stats.BlockNum)),
	})
	nodes[0].Data = append(nodes[0].Data, ranges...)
	return nodes
}

func (anal *anaylze) Nodes() []*process.AnalyzeInfo {
	return anal.analInfos
}

func validScopeCount(ss []*Scope) int {
	var cnt int

	for _, s := range ss {
		if s.IsEnd {
			continue
		}
		cnt++
	}
	return cnt
}

func extraRegisters(ss []*Scope, i int) []*process.WaitRegister {
	regs := make([]*process.WaitRegister, 0, len(ss))
	for _, s := range ss {
		if s.IsEnd {
			continue
		}
		regs = append(regs, s.Proc.Reg.MergeReceivers[i])
	}
	return regs
}

func dupType(typ *plan.Type) types.Type {
	return types.New(types.T(typ.Id), typ.Width, typ.Scale)
}

// Update the specific scopes's instruction to true
// then update the current idx
func (c *Compile) setAnalyzeCurrent(updateScopes []*Scope, nextId int) {
	if updateScopes != nil {
		updateScopesLastFlag(updateScopes)
	}

	c.anal.curr = nextId
	c.anal.isFirst = true
}

func updateScopesLastFlag(updateScopes []*Scope) {
	for _, s := range updateScopes {
		last := len(s.Instructions) - 1
		s.Instructions[last].IsLast = true
	}
}

func isLaunchMode(cnlist engine.Nodes) bool {
	for i := range cnlist {
		if !isSameCN(cnlist[0].Addr, cnlist[i].Addr) {
			return false
		}
	}
	return true
}

func isSameCN(addr string, currentCNAddr string) bool {
	// just a defensive judgment. In fact, we shouldn't have received such data.
	parts1 := strings.Split(addr, ":")
	if len(parts1) != 2 {
		logutil.Warnf("compileScope received a malformed cn address '%s', expected 'ip:port'", addr)
		return true
	}
	parts2 := strings.Split(currentCNAddr, ":")
	if len(parts2) != 2 {
		logutil.Warnf("compileScope received a malformed current-cn address '%s', expected 'ip:port'", currentCNAddr)
		return true
	}
	return parts1[0] == parts2[0]
}

func rowsetDataToVector(ctx context.Context, proc *process.Process, exprs []*plan.Expr) (*vector.Vector, error) {
	rowCount := len(exprs)
	if rowCount == 0 {
		return nil, moerr.NewInternalError(ctx, "rowsetData do not have rows")
	}
	var typ types.Type
	var vec *vector.Vector
	for _, e := range exprs {
		if e.Typ.Id != int32(types.T_any) {
			typ = plan2.MakeTypeByPlan2Type(e.Typ)
			vec = vector.NewVec(typ)
			break
		}
	}
	if vec == nil {
		typ = types.T_int32.ToType()
		vec = vector.NewVec(typ)
	}
	bat := batch.NewWithSize(0)
	bat.Zs = []int64{1}
	defer bat.Clean(proc.Mp())

	executors, err := colexec.NewExpressionExecutorsFromPlanExpressions(proc, exprs)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, e := range executors {
			e.Free()
		}
	}()

	for i, executor := range executors {
		tmp, err := executor.Eval(proc, []*batch.Batch{bat})
		if err != nil {
			return nil, err
		}
		if tmp.IsConstNull() || tmp.GetNulls().Contains(0) {
			vector.AppendFixed(vec, 0, true, proc.Mp())
			continue
		}
		switch typ.Oid {
		case types.T_bool:
			vector.AppendFixed(vec, vector.MustFixedCol[bool](tmp)[0], false, proc.Mp())
		case types.T_int8:
			vector.AppendFixed(vec, vector.MustFixedCol[int8](tmp)[0], false, proc.Mp())
		case types.T_int16:
			vector.AppendFixed(vec, vector.MustFixedCol[int16](tmp)[0], false, proc.Mp())
		case types.T_int32:
			vector.AppendFixed(vec, vector.MustFixedCol[int32](tmp)[0], false, proc.Mp())
		case types.T_int64:
			vector.AppendFixed(vec, vector.MustFixedCol[int64](tmp)[0], false, proc.Mp())
		case types.T_uint8:
			vector.AppendFixed(vec, vector.MustFixedCol[uint8](tmp)[0], false, proc.Mp())
		case types.T_uint16:
			vector.AppendFixed(vec, vector.MustFixedCol[uint16](tmp)[0], false, proc.Mp())
		case types.T_uint32:
			vector.AppendFixed(vec, vector.MustFixedCol[uint32](tmp)[0], false, proc.Mp())
		case types.T_uint64:
			vector.AppendFixed(vec, vector.MustFixedCol[uint64](tmp)[0], false, proc.Mp())
		case types.T_float32:
			vector.AppendFixed(vec, vector.MustFixedCol[float32](tmp)[0], false, proc.Mp())
		case types.T_float64:
			vector.AppendFixed(vec, vector.MustFixedCol[float64](tmp)[0], false, proc.Mp())
		case types.T_char, types.T_varchar, types.T_binary, types.T_varbinary, types.T_json, types.T_blob, types.T_text:
			vector.AppendBytes(vec, tmp.GetBytesAt(0), false, proc.Mp())
		case types.T_date:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Date](tmp)[0], false, proc.Mp())
		case types.T_datetime:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Datetime](tmp)[0], false, proc.Mp())
		case types.T_time:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Time](tmp)[0], false, proc.Mp())
		case types.T_timestamp:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Timestamp](tmp)[0], false, proc.Mp())
		case types.T_decimal64:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Decimal64](tmp)[0], false, proc.Mp())
		case types.T_decimal128:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Decimal128](tmp)[0], false, proc.Mp())
		case types.T_uuid:
			vector.AppendFixed(vec, vector.MustFixedCol[types.Uuid](tmp)[0], false, proc.Mp())
		default:
			return nil, moerr.NewNYI(ctx, fmt.Sprintf("expression %v can not eval to constant and append to rowsetData", exprs[i]))
		}
	}
	return vec, nil
}
