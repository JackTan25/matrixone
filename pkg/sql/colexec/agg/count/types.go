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

package count

import "github.com/matrixorigin/matrixone/pkg/container/types"

type Count struct {
	Da   []byte     // memory of Vs
	Vs   []int64    // the number of value of each group
	Ityp types.Type // input vector's type
	Otyp types.Type // output vector's type
}

type DistCount struct {
	Da   []byte     // memory of Vs
	Vs   []int64    // the number of value of each group
	Ityp types.Type // input vector's type
	Otyp types.Type // output vector's type
	Ms   []map[any]uint8
}
