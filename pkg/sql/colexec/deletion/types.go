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

package deletion

import (
	"github.com/matrixorigin/matrixone/pkg/pb/plan"
	"github.com/matrixorigin/matrixone/pkg/vm/engine"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

type Argument struct {
	Ts           uint64
	DeleteCtx    *DeleteCtx
	affectedRows uint64
	Engine       engine.Engine
	// when detele data in a remote CN,
	// IsRemote is true, and we need IBucket
	// and NBucket to know those data in batch
	// that we need to delete in this CN, because
	// we need to make sure one Block will be processed
	// by only one CN, this is useful for our compaction
	IsRemote bool
	IBucket  uint64
	NBucket  uint64
}

type DeleteCtx struct {
	CanTruncate           bool
	RowIdIdx              int               // The array index position of the rowid column
	PartitionTableIDs     []uint64          // Align array index with the partition number
	PartitionIndexInBatch int               // The array index position of the partition expression column
	PartitionSources      []engine.Relation // Align array index with the partition number
	Source                engine.Relation
	Ref                   *plan.ObjectRef
	AddAffectedRows       bool
}

func (arg *Argument) Free(proc *process.Process, pipelineFailed bool) {
}

func (arg *Argument) AffectedRows() uint64 {
	return arg.affectedRows
}
