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

package txnif

import (
	"context"
	"io"
	"sync"

	"github.com/matrixorigin/matrixone/pkg/objectio"

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/logstore/entry"

	"github.com/RoaringBitmap/roaring"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/iface/handle"
)

var (
	ErrTxnWWConflict = moerr.NewTxnWWConflictNoCtx()
)

type Txn2PC interface {
	PrepareRollback() error
	ApplyRollback() error
	PrePrepare() error
	PrepareCommit() error
	PreApplyCommit() error
	PrepareWAL() error
	ApplyCommit() error
}

type TxnReader interface {
	RLock()
	RUnlock()
	IsReplay() bool
	Is2PC() bool
	GetPKDedupSkip() PKDedupSkipScope
	GetID() string
	GetCtx() []byte
	GetStartTS() types.TS
	GetCommitTS() types.TS

	GetPrepareTS() types.TS
	GetParticipants() []uint64
	GetSnapshotTS() types.TS
	SetSnapshotTS(types.TS)
	HasSnapshotLag() bool
	IsVisible(o TxnReader) bool
	GetTxnState(waitIfcommitting bool) TxnState
	GetError() error
	GetStore() TxnStore
	String() string
	Repr() string
	GetLSN() uint64
	GetMemo() *TxnMemo

	SameTxn(txn TxnReader) bool
	CommitBefore(startTs types.TS) bool
	CommitAfter(startTs types.TS) bool
}

type TxnHandle interface {
	BindAccessInfo(tenantID, userID, roleID uint32)
	GetTenantID() uint32
	GetUserAndRoleID() (uint32, uint32)
	CreateDatabase(name, createSql, datTyp string) (handle.Database, error)
	CreateDatabaseWithCtx(ctx context.Context,
		name, createSql, datTyp string, id uint64) (handle.Database, error)
	DropDatabase(name string) (handle.Database, error)
	DropDatabaseByID(id uint64) (handle.Database, error)
	GetDatabase(name string) (handle.Database, error)
	GetDatabaseWithCtx(ctx context.Context, name string) (handle.Database, error)
	GetDatabaseByID(id uint64) (handle.Database, error)
	DatabaseNames() []string
}

type TxnChanger interface {
	sync.Locker
	RLock()
	RUnlock()
	ToCommittedLocked() error
	ToPreparingLocked(ts types.TS) error
	ToPrepared() error
	ToPreparedLocked() error
	ToRollbackedLocked() error

	ToRollbacking(ts types.TS) error
	ToRollbackingLocked(ts types.TS) error
	ToUnknownLocked()
	Prepare() (types.TS, error)
	Committing() error
	Commit() error
	Rollback() error
	SetCommitTS(cts types.TS) error
	SetPKDedupSkip(skip PKDedupSkipScope)
	SetParticipants(ids []uint64) error
	SetError(error)

	CommittingInRecovery() error
	CommitInRecovery() error
}

type TxnWriter interface {
	LogTxnEntry(dbId, tableId uint64, entry TxnEntry, readed []*common.ID) error
	LogTxnState(sync bool) (entry.Entry, error)
}

type TxnAsyncer interface {
	WaitDone(error, bool) error
	WaitPrepared() error
}

type TxnTest interface {
	MockIncWriteCnt() int
	MockStartTS(types.TS)
	SetPrepareCommitFn(func(AsyncTxn) error)
	SetPrepareRollbackFn(func(AsyncTxn) error)
	SetApplyCommitFn(func(AsyncTxn) error)
	SetApplyRollbackFn(func(AsyncTxn) error)
}

type TxnUnsafe interface {
	UnsafeGetDatabase(id uint64) (h handle.Database, err error)
	UnsafeGetRelation(dbId, tableId uint64) (h handle.Relation, err error)
}

type AsyncTxn interface {
	TxnUnsafe
	TxnTest
	Txn2PC
	TxnHandle
	TxnAsyncer
	TxnReader
	TxnWriter
	TxnChanger
}

type DeleteChain interface {
	sync.Locker
	RLock()
	RUnlock()
	RemoveNodeLocked(DeleteNode)

	AddNodeLocked(txn AsyncTxn, deleteType handle.DeleteType) DeleteNode
	AddMergeNode() DeleteNode

	PrepareRangeDelete(start, end uint32, ts types.TS) error
	DepthLocked() int
	CollectDeletesLocked(txn TxnReader, rwlocker *sync.RWMutex) (DeleteNode, error)
}
type BaseNode[T any] interface {
	Update(o T)
	CloneData() T
	CloneAll() T
}

type MVCCNode[T any] interface {
	BaseMVCCNode
	BaseNode[T]
}

type BaseMVCCNode interface {
	String() string
	IsNil() bool

	IsVisibleByTS(ts types.TS) (visible bool)
	IsVisible(txn TxnReader) (visible bool)
	CheckConflict(txn TxnReader) error

	PreparedIn(minTS, maxTS types.TS) (in, before bool)
	CommittedIn(minTS, maxTS types.TS) (in, before bool)
	NeedWaitCommitting(ts types.TS) (bool, TxnReader)
	IsSameTxn(txn TxnReader) bool
	IsActive() bool
	IsCommitting() bool
	IsCommitted() bool
	IsAborted() bool
	Set1PC()
	Is1PC() bool

	GetEnd() types.TS
	GetStart() types.TS
	GetPrepare() types.TS
	GetTxn() TxnReader

	ApplyCommit() (err error)
	ApplyRollback() (err error)
	PrepareCommit() (err error)
	PrepareRollback() (err error)

	WriteTo(w io.Writer) (n int64, err error)
	ReadFrom(r io.Reader) (n int64, err error)
}
type AppendNode interface {
	BaseMVCCNode
	TxnEntry
	GetStartRow() uint32
	GetMaxRow() uint32
}

type DeleteNode interface {
	BaseMVCCNode
	TxnEntry
	StringLocked() string
	GetChain() DeleteChain
	DeletedRows() []uint32
	RangeDeleteLocked(start, end uint32)
	GetCardinalityLocked() uint32
	IsDeletedLocked(row uint32) bool
	GetRowMaskRefLocked() *roaring.Bitmap
	OnApply() error
}

type TxnStore interface {
	io.Closer
	Txn2PC
	TxnUnsafe
	WaitPrepared() error
	BindTxn(AsyncTxn)
	GetLSN() uint64

	BatchDedup(dbId, id uint64, pk containers.Vector) error

	Append(dbId, id uint64, data *containers.Batch) error
	AddBlksWithMetaLoc(dbId, id uint64, metaLocs []objectio.Location) error

	RangeDelete(id *common.ID, start, end uint32, dt handle.DeleteType) error
	GetByFilter(dbId uint64, id uint64, filter *handle.Filter) (*common.ID, uint32, error)
	GetValue(id *common.ID, row uint32, col uint16) (any, bool, error)

	CreateRelation(dbId uint64, def any) (handle.Relation, error)
	CreateRelationWithTableId(dbId uint64, tableId uint64, def any) (handle.Relation, error)
	DropRelationByName(dbId uint64, name string) (handle.Relation, error)
	DropRelationByID(dbId uint64, tid uint64) (handle.Relation, error)
	GetRelationByName(dbId uint64, name string) (handle.Relation, error)
	GetRelationByID(dbId uint64, tid uint64) (handle.Relation, error)

	CreateDatabase(name, createSql, datTyp string) (handle.Database, error)
	CreateDatabaseWithID(name, createSql, datTyp string, id uint64) (handle.Database, error)
	GetDatabase(name string) (handle.Database, error)
	GetDatabaseByID(id uint64) (handle.Database, error)
	DropDatabase(name string) (handle.Database, error)
	DropDatabaseByID(id uint64) (handle.Database, error)
	DatabaseNames() []string

	GetSegment(id *common.ID) (handle.Segment, error)
	CreateSegment(dbId, tid uint64, is1PC bool) (handle.Segment, error)
	CreateNonAppendableSegment(dbId, tid uint64, is1PC bool) (handle.Segment, error)
	CreateBlock(id *common.ID, is1PC bool) (handle.Block, error)
	GetBlock(id *common.ID) (handle.Block, error)
	CreateNonAppendableBlock(id *common.ID, opts *objectio.CreateBlockOpt) (handle.Block, error)
	SoftDeleteSegment(id *common.ID) error
	SoftDeleteBlock(id *common.ID) error
	UpdateMetaLoc(id *common.ID, metaLoc objectio.Location) (err error)
	UpdateDeltaLoc(id *common.ID, deltaLoc objectio.Location) (err error)

	AddTxnEntry(TxnEntryType, TxnEntry)

	LogTxnEntry(dbId, tableId uint64, entry TxnEntry, readed []*common.ID) error
	LogTxnState(sync bool) (entry.Entry, error)
	DoneWaitEvent(cnt int)
	AddWaitEvent(cnt int)

	IsReadonly() bool
	IncreateWriteCnt() int
	ObserveTxn(
		visitDatabase func(db any),
		visitTable func(tbl any),
		rotateTable func(dbName, tblName string, dbid, tid uint64),
		visitMetadata func(block any),
		visitSegment func(seg any),
		visitAppend func(bat any),
		visitDelete func(deletes DeleteNode))
	GetTransactionType() TxnType
}

type TxnType int8

const (
	TxnType_Normal = iota
	TxnType_Heartbeat
)

type TxnEntryType int16

type TxnEntry interface {
	PrepareCommit() error
	PrepareRollback() error
	ApplyCommit() error
	ApplyRollback() error
	MakeCommand(uint32) (TxnCmd, error)
	Is1PC() bool
	Set1PC()
}
