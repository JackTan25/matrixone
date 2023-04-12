// Copyright 2022 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package mergedelete

import (
	"bytes"

	"github.com/matrixorigin/matrixone/pkg/catalog"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

func String(_ any, buf *bytes.Buffer) {
	buf.WriteString(" MergeS3DeleteInfo ")
}

func Prepare(proc *process.Process, arg any) error {
	return nil
}

func Call(idx int, proc *process.Process, arg any, isFirst bool, isLast bool) (bool, error) {
	var err error
	ap := arg.(*Argument)
	bat := proc.Reg.InputBatch
	if bat == nil {
		// here means the delete is over, we should start to do
		// compaction here
		ap.DelSource.Delete(proc.Ctx, nil, catalog.BlockMeta_ID)
		return true, nil
	}

	if len(bat.Zs) == 0 {
		return false, nil
	}
	// val, err := strconv.ParseUint(strings.Split(string(metaLocs[i]), ":")[2], 0, 64)
	// blkId,metaLoc,type
	err = ap.DelSource.Delete(proc.Ctx, bat, catalog.BlockMeta_ID)
	if err != nil {
		return false, err
	}
	ap.AffectedRows += uint64(bat.Length())
	return false, nil
}
