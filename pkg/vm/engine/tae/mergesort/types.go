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

package mergesort

import (
	"bytes"

	"github.com/matrixorigin/matrixone/pkg/container/types"
)

// uuid && decimal
type Lter[T any] interface {
	Lt(b T) bool
}

type LessFunc[T any] func(a, b T) bool

func numericLess[T types.OrderedT](a, b T) bool { return a < b }
func boolLess(a, b bool) bool                   { return !a && b }
func ltTypeLess[T Lter[T]](a, b T) bool         { return a.Lt(b) }

// it seems that go has no const generic type, handle these types respectively
func tsLess(a, b types.TS) bool           { return bytes.Compare(a[:], b[:]) < 0 }
func rowidLess(a, b types.Rowid) bool     { return bytes.Compare(a[:], b[:]) < 0 }
func blockidLess(a, b types.Blockid) bool { return bytes.Compare(a[:], b[:]) < 0 }
func bytesLess(a, b []byte) bool          { return bytes.Compare(a, b) < 0 }

const nullFirst = true

type SortElem[T any] struct {
	data   T
	isNull bool
	idx    uint32
}

type SortSlice[T any] struct {
	lessFunc LessFunc[T]
	s        []SortElem[T]
}

func NewSortSlice[T any](n int, lessFunc LessFunc[T]) SortSlice[T] {
	return SortSlice[T]{
		lessFunc: lessFunc,
		s:        make([]SortElem[T], 0, n),
	}
}

func (x *SortSlice[T]) Less(i, j int) bool {
	a, b := x.s[i], x.s[j]
	if !a.isNull && !b.isNull {
		return x.lessFunc(a.data, b.data)
	}
	// if nullFirst = true， then
	// null null = false
	// null x    = true
	// x    null = false
	// if nullFirst = false, then
	// null null = false
	// null x    = false
	// x    null = true
	if a.isNull && b.isNull {
		return false
	} else if a.isNull {
		return nullFirst
	} else {
		return !nullFirst
	}
}
func (x *SortSlice[T]) Swap(i, j int)           { x.s[i], x.s[j] = x.s[j], x.s[i] }
func (x *SortSlice[T]) AsSlice() []SortElem[T]  { return x.s }
func (x *SortSlice[T]) Append(elem SortElem[T]) { x.s = append(x.s, elem) }

type HeapElem[T any] struct {
	data   T
	isNull bool
	src    uint32
	next   uint32
}

type HeapSlice[T any] struct {
	lessFunc LessFunc[T]
	s        []HeapElem[T]
}

func NewHeapSlice[T any](n int, lessFunc LessFunc[T]) HeapSlice[T] {
	return HeapSlice[T]{
		lessFunc: lessFunc,
		s:        make([]HeapElem[T], 0, n),
	}
}

func (x *HeapSlice[T]) Less(i, j int) bool {
	a, b := x.s[i], x.s[j]
	if !a.isNull && !b.isNull {
		return x.lessFunc(a.data, b.data)
	}
	if a.isNull && b.isNull {
		return false
	} else if a.isNull {
		return nullFirst
	} else {
		return !nullFirst
	}
}
func (x *HeapSlice[T]) Swap(i, j int)           { x.s[i], x.s[j] = x.s[j], x.s[i] }
func (x *HeapSlice[T]) AsSlice() []HeapElem[T]  { return x.s }
func (x *HeapSlice[T]) Append(elem HeapElem[T]) { x.s = append(x.s, elem) }
