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

package tables

import (
	"context"

	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/fileservice"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/index/indexwrapper"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/model"

	"github.com/matrixorigin/matrixone/pkg/objectio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/blockio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
)

func LoadPersistedColumnData(
	fs *objectio.ObjectFS,
	id *common.ID,
	def *catalog.ColDef,
	location objectio.Location,
) (vec containers.Vector, err error) {
	if def.IsPhyAddr() {
		return model.PreparePhyAddrData(&id.BlockID, 0, location.Rows())
	}
	bat, err := blockio.LoadColumns(context.Background(), []uint16{uint16(def.SeqNum)}, []types.Type{def.Type}, fs.Service, location, nil)
	if err != nil {
		return
	}
	return containers.ToDNVector(bat.Vecs[0]), nil
}

func ReadPersistedBlockRow(location objectio.Location) int {
	return int(location.Rows())
}

func LoadPersistedDeletes(
	pkName string,
	fs *objectio.ObjectFS,
	location objectio.Location) (bat *containers.Batch, err error) {
	movbat, err := blockio.LoadColumns(context.Background(), []uint16{0, 1, 2, 3}, nil, fs.Service, location, nil)
	if err != nil {
		return
	}
	bat = containers.NewBatch()
	colNames := []string{catalog.PhyAddrColumnName, catalog.AttrCommitTs, pkName, catalog.AttrAborted}
	for i := 0; i < 4; i++ {
		bat.AddVector(colNames[i], containers.ToDNVector(movbat.Vecs[i]))
	}
	return
}

// func MakeBFLoader(
// 	meta *catalog.BlockEntry,
// 	bf objectio.BloomFilter,
// 	cache model.LRUCache,
// 	fs fileservice.FileService,
// ) indexwrapper.Loader {
// 	return func(ctx context.Context) ([]byte, error) {
// 		location := meta.GetMetaLoc()
// 		var err error
// 		if len(bf) == 0 {
// 			if bf, err = LoadBF(ctx, location, cache, fs, false); err != nil {
// 				return nil, err
// 			}
// 		}
// 		return bf.GetBloomFilter(uint32(location.ID())), nil
// 	}
// }

func MakeImmuIndex(
	ctx context.Context,
	meta *catalog.BlockEntry,
	bf objectio.BloomFilter,
	cache model.LRUCache,
	fs fileservice.FileService,
) (idx indexwrapper.ImmutIndex, err error) {
	pkZM, err := meta.GetPKZoneMap(ctx, fs)
	if err != nil {
		return
	}
	idx = indexwrapper.NewImmutIndex(
		*pkZM, bf, meta.GetMetaLoc(), cache, fs,
	)
	return
}
