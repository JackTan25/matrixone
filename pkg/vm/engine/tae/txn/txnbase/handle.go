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

package txnbase

import (
	"context"

	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/objectio"
	apipb "github.com/matrixorigin/matrixone/pkg/pb/api"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/txnif"
)

type TxnDatabase struct {
	Txn txnif.AsyncTxn
}

type TxnRelation struct {
	Txn txnif.AsyncTxn
	DB  handle.Database
}

type TxnSegment struct {
	Txn txnif.AsyncTxn
	Rel handle.Relation
}

type TxnBlock struct {
	Txn txnif.AsyncTxn
	Seg handle.Segment
}

var _ handle.Relation = &TxnRelation{}

func (db *TxnDatabase) GetID() uint64                                                   { return 0 }
func (db *TxnDatabase) GetName() string                                                 { return "" }
func (db *TxnDatabase) String() string                                                  { return "" }
func (db *TxnDatabase) Close() error                                                    { return nil }
func (db *TxnDatabase) CreateRelation(def any) (rel handle.Relation, err error)         { return }
func (db *TxnDatabase) DropRelationByName(name string) (rel handle.Relation, err error) { return }
func (db *TxnDatabase) GetRelationByName(name string) (rel handle.Relation, err error)  { return }
func (db *TxnDatabase) UnsafeGetRelation(id uint64) (rel handle.Relation, err error)    { return }
func (db *TxnDatabase) RelationCnt() int64                                              { return 0 }
func (db *TxnDatabase) Relations() (rels []handle.Relation)                             { return }
func (db *TxnDatabase) MakeRelationIt() (it handle.RelationIt)                          { return }
func (db *TxnDatabase) GetMeta() any                                                    { return nil }

func (rel *TxnRelation) SimplePPString(_ common.PPLevel) string { return "" }
func (rel *TxnRelation) String() string                         { return "" }
func (rel *TxnRelation) Close() error                           { return nil }
func (rel *TxnRelation) ID() uint64                             { return 0 }
func (rel *TxnRelation) Rows() int64                            { return 0 }
func (rel *TxnRelation) Size(attr string) int64                 { return 0 }
func (rel *TxnRelation) GetCardinality(attr string) int64       { return 0 }
func (rel *TxnRelation) Schema() any                            { return nil }
func (rel *TxnRelation) MakeSegmentIt() handle.SegmentIt        { return nil }
func (rel *TxnRelation) MakeSegmentItOnSnap() handle.SegmentIt  { return nil }
func (rel *TxnRelation) MakeBlockIt() handle.BlockIt            { return nil }
func (rel *TxnRelation) BatchDedup(col containers.Vector) error { return nil }
func (rel *TxnRelation) Append(data *containers.Batch) error    { return nil }
func (rel *TxnRelation) AddBlksWithMetaLoc([]objectio.Location) error {
	return nil
}
func (rel *TxnRelation) GetMeta() any                                                        { return nil }
func (rel *TxnRelation) GetDB() (handle.Database, error)                                     { return nil, nil }
func (rel *TxnRelation) GetSegment(id *types.Segmentid) (seg handle.Segment, err error)      { return }
func (rel *TxnRelation) SoftDeleteSegment(id *types.Segmentid) (err error)                   { return }
func (rel *TxnRelation) CreateSegment(bool) (seg handle.Segment, err error)                  { return }
func (rel *TxnRelation) CreateNonAppendableSegment(bool) (seg handle.Segment, err error)     { return }
func (rel *TxnRelation) GetValue(*common.ID, uint32, uint16) (v any, isNull bool, err error) { return }
func (rel *TxnRelation) GetValueByPhyAddrKey(any, int) (v any, isNull bool, err error)       { return }
func (rel *TxnRelation) Update(*common.ID, uint32, uint16, any, bool) (err error)            { return }
func (rel *TxnRelation) DeleteByPhyAddrKey(any) (err error)                                  { return }
func (rel *TxnRelation) DeleteByPhyAddrKeys(containers.Vector) (err error)                   { return }
func (rel *TxnRelation) RangeDelete(*common.ID, uint32, uint32, handle.DeleteType) (err error) {
	return
}
func (rel *TxnRelation) GetByFilter(*handle.Filter) (id *common.ID, offset uint32, err error) { return }
func (rel *TxnRelation) GetValueByFilter(filter *handle.Filter, col int) (v any, isNull bool, err error) {
	return
}
func (rel *TxnRelation) UpdateByFilter(filter *handle.Filter, col uint16, v any, isNull bool) (err error) {
	return
}
func (rel *TxnRelation) DeleteByFilter(filter *handle.Filter) (err error) { return }
func (rel *TxnRelation) LogTxnEntry(entry txnif.TxnEntry, readed []*common.ID) (err error) {
	return
}
func (rel *TxnRelation) AlterTable(context.Context, *apipb.AlterTableReq) (err error) { return }

func (seg *TxnSegment) Reset() {
	seg.Txn = nil
	seg.Rel = nil
}
func (seg *TxnSegment) GetMeta() any                     { return nil }
func (seg *TxnSegment) String() string                   { return "" }
func (seg *TxnSegment) Close() error                     { return nil }
func (seg *TxnSegment) GetID() uint64                    { return 0 }
func (seg *TxnSegment) MakeBlockIt() (it handle.BlockIt) { return }

// func (seg *TxnSegment) GetByFilter(*handle.Filter) (id *common.ID, offset uint32, err error) {
// 	return
// }

func (seg *TxnSegment) GetRelation() (rel handle.Relation)                                { return }
func (seg *TxnSegment) Update(uint64, uint32, uint16, any) (err error)                    { return }
func (seg *TxnSegment) RangeDelete(uint64, uint32, uint32, handle.DeleteType) (err error) { return }

func (seg *TxnSegment) PushDeleteOp(handle.Filter) (err error)                  { return }
func (seg *TxnSegment) PushUpdateOp(handle.Filter, string, any) (err error)     { return }
func (seg *TxnSegment) SoftDeleteBlock(id types.Blockid) (err error)            { return }
func (seg *TxnSegment) GetBlock(id uint64) (blk handle.Block, err error)        { return }
func (seg *TxnSegment) CreateBlock() (blk handle.Block, err error)              { return }
func (seg *TxnSegment) CreateNonAppendableBlock() (blk handle.Block, err error) { return }
func (seg *TxnSegment) BatchDedup(containers.Vector) (err error)                { return }

// func (blk *TxnBlock) IsAppendable() bool                                   { return true }

func (blk *TxnBlock) Reset() {
	blk.Txn = nil
	blk.Seg = nil
}
func (blk *TxnBlock) GetTotalChanges() int                                  { return 0 }
func (blk *TxnBlock) IsAppendableBlock() bool                               { return true }
func (blk *TxnBlock) Fingerprint() *common.ID                               { return &common.ID{} }
func (blk *TxnBlock) Rows() int                                             { return 0 }
func (blk *TxnBlock) ID() uint64                                            { return 0 }
func (blk *TxnBlock) String() string                                        { return "" }
func (blk *TxnBlock) Close() error                                          { return nil }
func (blk *TxnBlock) GetMeta() any                                          { return nil }
func (blk *TxnBlock) GetByFilter(*handle.Filter) (offset uint32, err error) { return }

func (blk *TxnBlock) GetSegment() (seg handle.Segment) { return }

func (blk *TxnBlock) Append(*containers.Batch, uint32) (n uint32, err error)    { return }
func (blk *TxnBlock) Update(uint32, uint16, any) (err error)                    { return }
func (blk *TxnBlock) RangeDelete(uint32, uint32, handle.DeleteType) (err error) { return }
func (blk *TxnBlock) PushDeleteOp(handle.Filter) (err error)                    { return }
func (blk *TxnBlock) PushUpdateOp(handle.Filter, string, any) (err error)       { return }
