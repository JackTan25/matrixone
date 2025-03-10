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

package data

import (
	"time"

	"github.com/matrixorigin/matrixone/pkg/objectio"

	"github.com/matrixorigin/matrixone/pkg/container/types"

	"github.com/RoaringBitmap/roaring"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/txnif"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/index"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/model"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/tasks"
)

type CheckpointUnit interface {
	MutationInfo() string
	RunCalibration() int
	EstimateScore(time.Duration, bool) int
	BuildCompactionTaskFactory() (tasks.TxnTaskFactory, tasks.TaskType, []common.ID, error)
}

type BlockAppender interface {
	GetID() *common.ID
	GetMeta() any
	IsSameColumns(otherSchema any /*avoid import cycle*/) bool
	PrepareAppend(rows uint32,
		txn txnif.AsyncTxn) (
		node txnif.AppendNode, created bool, n uint32, err error)
	ApplyAppend(bat *containers.Batch,
		txn txnif.AsyncTxn,
	) (int, error)
	IsAppendable() bool
	ReplayAppend(bat *containers.Batch,
		txn txnif.AsyncTxn) (int, error)
	Close()
}

type BlockReplayer interface {
	OnReplayDelete(node txnif.DeleteNode) (err error)
	OnReplayAppend(node txnif.AppendNode) (err error)
	OnReplayAppendPayload(bat *containers.Batch) (err error)
}

type Block interface {
	CheckpointUnit
	BlockReplayer

	DeletesInfo() string

	GetRowsOnReplay() uint64
	GetID() *common.ID
	IsAppendable() bool
	PrepareCompact() bool

	Rows() int
	GetColumnDataById(txn txnif.AsyncTxn, readSchema any /*avoid import cycle*/, colIdx int) (*model.ColumnView, error)
	GetColumnDataByIds(txn txnif.AsyncTxn, readSchema any, colIdxes []int) (*model.BlockView, error)
	Prefetch(idxes []uint16) error
	GetMeta() any

	MakeAppender() (BlockAppender, error)
	RangeDelete(txn txnif.AsyncTxn, start, end uint32, dt handle.DeleteType) (txnif.DeleteNode, error)

	GetTotalChanges() int
	CollectChangesInRange(startTs, endTs types.TS) (*model.BlockView, error)

	// check wether any delete intents with prepared ts within [from, to]
	HasDeleteIntentsPreparedIn(from, to types.TS) bool

	DataCommittedBefore(ts types.TS) bool
	BatchDedup(txn txnif.AsyncTxn,
		pks containers.Vector,
		pksZM index.ZM,
		rowmask *roaring.Bitmap,
		precommit bool,
		bf objectio.BloomFilter) error
	//TODO::
	//BatchDedupByMetaLoc(txn txnif.AsyncTxn, fs *objectio.ObjectFS,
	//	metaLoc objectio.Location, rowmask *roaring.Bitmap, precommit bool) error

	GetByFilter(txn txnif.AsyncTxn, filter *handle.Filter) (uint32, error)
	GetValue(txn txnif.AsyncTxn, readSchema any, row, col int) (any, bool, error)
	Foreach(colIdx int, op func(v any, isNull bool, row int) error, sels *roaring.Bitmap) error
	PPString(level common.PPLevel, depth int, prefix string) string

	Init() error
	TryUpgrade() error
	CollectAppendInRange(start, end types.TS, withAborted bool) (*containers.BatchWithVersion, error)
	CollectDeleteInRange(start, end types.TS, withAborted bool) (*containers.Batch, error)
	// GetAppendNodeByRow(row uint32) (an txnif.AppendNode)
	// GetDeleteNodeByRow(row uint32) (an txnif.DeleteNode)
	GetFs() *objectio.ObjectFS
	FreezeAppend()

	Close()
}
