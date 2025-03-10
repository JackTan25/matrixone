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

package db

import (
	"testing"

	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/options"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tables/jobs"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tasks"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/testutils"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/testutils/config"
	"github.com/stretchr/testify/assert"
)

func TestCheckpoint1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	opts := config.WithQuickScanAndCKPOpts(nil)
	db := initDB(t, opts)
	defer db.Close()
	schema := catalog.MockSchema(13, 12)
	schema.BlockMaxRows = 1000
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	{
		txn, _ := db.StartTxn(nil)
		database, _ := txn.CreateDatabase("db", "", "")
		rel, _ := database.CreateRelation(schema)
		err := rel.Append(bat)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit())
	}
	{
		txn, _ := db.StartTxn(nil)
		database, _ := txn.GetDatabase("db")
		rel, _ := database.GetRelationByName(schema.Name)
		it := rel.MakeBlockIt()
		blk := it.GetBlock()
		err := blk.RangeDelete(3, 3, handle.DT_Normal)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit())
	}

	blockCnt := 0
	fn := func() bool {
		blockCnt = 0
		blockFn := func(entry *catalog.BlockEntry) error {
			blockCnt++
			return nil
		}
		processor := new(catalog.LoopProcessor)
		processor.BlockFn = blockFn
		err := db.Opts.Catalog.RecurLoop(processor)
		assert.NoError(t, err)
		return blockCnt == 2+3
	}
	testutils.WaitExpect(1000, fn)
	fn()
	assert.Equal(t, 2+3, blockCnt)
}

func TestCheckpoint2(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	opts := new(options.Options)
	opts.CacheCfg = new(options.CacheCfg)
	opts.CacheCfg.IndexCapacity = 1000000
	// opts.CheckpointCfg = new(options.CheckpointCfg)
	// opts.CheckpointCfg.ScannerInterval = 10
	// opts.CheckpointCfg.ExecutionLevels = 2
	// opts.CheckpointCfg.ExecutionInterval = 1
	tae := initDB(t, opts)
	defer tae.Close()
	schema1 := catalog.MockSchema(4, 2)
	schema1.BlockMaxRows = 10
	schema1.SegmentMaxBlocks = 2
	schema2 := catalog.MockSchema(4, 2)
	schema2.BlockMaxRows = 10
	schema2.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema1, int(schema1.BlockMaxRows*2))
	defer bat.Close()
	bats := bat.Split(10)
	var (
		meta1 *catalog.TableEntry
		meta2 *catalog.TableEntry
	)
	{
		txn, _ := tae.StartTxn(nil)
		db, _ := txn.CreateDatabase("db", "", "")
		rel1, _ := db.CreateRelation(schema1)
		rel2, _ := db.CreateRelation(schema2)
		meta1 = rel1.GetMeta().(*catalog.TableEntry)
		meta2 = rel2.GetMeta().(*catalog.TableEntry)
		t.Log(meta1.String())
		t.Log(meta2.String())
		assert.Nil(t, txn.Commit())
	}
	for i, data := range bats[0:8] {
		var name string
		if i%2 == 0 {
			name = schema1.Name
		} else {
			name = schema2.Name
		}
		appendClosure(t, data, name, tae, nil)()
	}
	var meta *catalog.BlockEntry
	testutils.WaitExpect(1000, func() bool {
		return tae.Wal.GetPenddingCnt() == 9
	})
	assert.Equal(t, uint64(9), tae.Wal.GetPenddingCnt())
	t.Log(tae.Wal.GetPenddingCnt())
	appendClosure(t, bats[8], schema1.Name, tae, nil)()
	// t.Log(tae.MTBufMgr.String())

	{
		txn, _ := tae.StartTxn(nil)
		db, err := txn.GetDatabase("db")
		assert.Nil(t, err)
		rel, err := db.GetRelationByName(schema1.Name)
		assert.Nil(t, err)
		it := rel.MakeBlockIt()
		blk := it.GetBlock()
		meta = blk.GetMeta().(*catalog.BlockEntry)
		assert.Equal(t, 10, blk.Rows())
		task, err := jobs.NewCompactBlockTask(tasks.WaitableCtx, txn, meta, tae.Scheduler)
		assert.Nil(t, err)
		err = tae.Scheduler.Schedule(task)
		assert.Nil(t, err)
		err = task.WaitDone()
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit())
	}

	// testutils.WaitExpect(1000, func() bool {
	// 	return tae.Wal.GetPenddingCnt() == 1
	// })
	// t.Log(tae.Wal.GetPenddingCnt())
	// err := meta.GetBlockData().Destroy()
	// assert.Nil(t, err)
	// task, err := tae.Scheduler.ScheduleScopedFn(tasks.WaitableCtx, tasks.CheckpointTask, nil, tae.Catalog.CheckpointClosure(tae.Scheduler.GetCheckpointTS()))
	// assert.Nil(t, err)
	// err = task.WaitDone()
	// assert.Nil(t, err)
	// testutils.WaitExpect(1000, func() bool {
	// 	return tae.Wal.GetPenddingCnt() == 4
	// })
	// t.Log(tae.Wal.GetPenddingCnt())
	// assert.Equal(t, uint64(4), tae.Wal.GetPenddingCnt())
}

func TestSchedule1(t *testing.T) {
	defer testutils.AfterTest(t)()
	testutils.EnsureNoLeak(t)
	db := initDB(t, nil)
	schema := catalog.MockSchema(13, 12)
	schema.BlockMaxRows = 10
	schema.SegmentMaxBlocks = 2
	bat := catalog.MockBatch(schema, int(schema.BlockMaxRows))
	defer bat.Close()
	{
		txn, _ := db.StartTxn(nil)
		database, _ := txn.CreateDatabase("db", "", "")
		rel, _ := database.CreateRelation(schema)
		err := rel.Append(bat)
		assert.Nil(t, err)
		assert.Nil(t, txn.Commit())
	}
	compactBlocks(t, 0, db, "db", schema, false)
	t.Log(db.Opts.Catalog.SimplePPString(common.PPL1))
	db.Close()
}
