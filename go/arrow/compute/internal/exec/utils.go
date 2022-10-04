// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exec

import (
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"unsafe"

	"github.com/apache/arrow/go/v10/arrow"
	"github.com/apache/arrow/go/v10/arrow/array"
	"github.com/apache/arrow/go/v10/arrow/decimal128"
	"github.com/apache/arrow/go/v10/arrow/decimal256"
	"github.com/apache/arrow/go/v10/arrow/float16"
	"github.com/apache/arrow/go/v10/arrow/memory"
	"golang.org/x/exp/constraints"
	"golang.org/x/exp/slices"
)

// IntTypes is a type constraint for raw values represented as signed
// integer types by Arrow. We aren't just using constraints.Signed
// because we don't want to include the raw `int` type here whose size
// changes based on the architecture (int32 on 32-bit architectures and
// int64 on 64-bit architectures).
//
// This will also cover types like MonthInterval or the time types
// as their underlying types are int32 and int64 which will get covered
// by using the ~
type IntTypes interface {
	~int8 | ~int16 | ~int32 | ~int64
}

// UintTypes is a type constraint for raw values represented as unsigned
// integer types by Arrow. We aren't just using constraints.Unsigned
// because we don't want to include the raw `uint` type here whose size
// changes based on the architecture (uint32 on 32-bit architectures and
// uint64 on 64-bit architectures). We also don't want to include uintptr
type UintTypes interface {
	~uint8 | ~uint16 | ~uint32 | ~uint64
}

// FloatTypes is a type constraint for raw values for representing
// floating point values in Arrow. This consists of constraints.Float and
// float16.Num
type FloatTypes interface {
	float16.Num | constraints.Float
}

// NumericTypes is a type constraint for just signed/unsigned integers
// and float32/float64.
type NumericTypes interface {
	IntTypes | UintTypes | constraints.Float
}

// DecimalTypes is a type constraint for raw values representing larger
// decimal type values in Arrow, specifically decimal128 and decimal256.
type DecimalTypes interface {
	decimal128.Num | decimal256.Num
}

// FixedWidthTypes is a type constraint for raw values in Arrow that
// can be represented as FixedWidth byte slices. Specifically this is for
// using Go generics to easily re-type a byte slice to a properly-typed
// slice. Booleans are excluded here since they are represented by Arrow
// as a bitmap and thus the buffer can't be just reinterpreted as a []bool
type FixedWidthTypes interface {
	IntTypes | UintTypes |
		FloatTypes | DecimalTypes |
		arrow.DayTimeInterval | arrow.MonthDayNanoInterval
}

type TemporalTypes interface {
	arrow.Date32 | arrow.Date64 | arrow.Time32 | arrow.Time64 |
		arrow.Timestamp | arrow.Duration | arrow.DayTimeInterval |
		arrow.MonthInterval | arrow.MonthDayNanoInterval
}

func GetValues[T FixedWidthTypes](data arrow.ArrayData, i int) []T {
	if data.Buffers()[i] == nil || data.Buffers()[i].Len() == 0 {
		return nil
	}
	ret := unsafe.Slice((*T)(unsafe.Pointer(&data.Buffers()[i].Bytes()[0])), data.Offset()+data.Len())
	return ret[data.Offset():]
}

// GetSpanValues returns a properly typed slice bye reinterpreting
// the buffer at index i using unsafe.Slice. This will take into account
// the offset of the given ArraySpan.
func GetSpanValues[T FixedWidthTypes](span *ArraySpan, i int) []T {
	if len(span.Buffers[i].Buf) == 0 {
		return nil
	}
	ret := unsafe.Slice((*T)(unsafe.Pointer(&span.Buffers[i].Buf[0])), span.Offset+span.Len)
	return ret[span.Offset:]
}

// GetSpanOffsets is like GetSpanValues, except it is only for int32
// or int64 and adds the additional 1 expected value for an offset
// buffer (ie. len(output) == span.Len+1)
func GetSpanOffsets[T int32 | int64](span *ArraySpan, i int) []T {
	ret := unsafe.Slice((*T)(unsafe.Pointer(&span.Buffers[i].Buf[0])), span.Offset+span.Len+1)
	return ret[span.Offset:]
}

func GetBytes[T FixedWidthTypes](in []T) []byte {
	var z T
	return unsafe.Slice((*byte)(unsafe.Pointer(&in[0])), len(in)*int(unsafe.Sizeof(z)))
}

func GetData[T FixedWidthTypes](in []byte) []T {
	var z T
	return unsafe.Slice((*T)(unsafe.Pointer(&in[0])), len(in)/int(unsafe.Sizeof(z)))
}

func Min[T constraints.Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}

// OptionsInit should be used in the case where a KernelState is simply
// represented with a specific type by value (instead of pointer).
// This will initialize the KernelState as a value-copied instance of
// the passed in function options argument to ensure separation
// and allow the kernel to manipulate the options if necessary without
// any negative consequences since it will have its own copy of the options.
func OptionsInit[T any](_ *KernelCtx, args KernelInitArgs) (KernelState, error) {
	if opts, ok := args.Options.(*T); ok {
		return *opts, nil
	}

	return nil, fmt.Errorf("%w: attempted to initialize kernel state from invalid function options",
		arrow.ErrInvalid)
}

var typMap = map[reflect.Type]arrow.DataType{
	reflect.TypeOf(int8(0)):         arrow.PrimitiveTypes.Int8,
	reflect.TypeOf(int16(0)):        arrow.PrimitiveTypes.Int16,
	reflect.TypeOf(int32(0)):        arrow.PrimitiveTypes.Int32,
	reflect.TypeOf(int64(0)):        arrow.PrimitiveTypes.Int64,
	reflect.TypeOf(uint8(0)):        arrow.PrimitiveTypes.Uint8,
	reflect.TypeOf(uint16(0)):       arrow.PrimitiveTypes.Uint16,
	reflect.TypeOf(uint32(0)):       arrow.PrimitiveTypes.Uint32,
	reflect.TypeOf(uint64(0)):       arrow.PrimitiveTypes.Uint64,
	reflect.TypeOf(float32(0)):      arrow.PrimitiveTypes.Float32,
	reflect.TypeOf(float64(0)):      arrow.PrimitiveTypes.Float64,
	reflect.TypeOf(string("")):      arrow.BinaryTypes.String,
	reflect.TypeOf(arrow.Date32(0)): arrow.FixedWidthTypes.Date32,
	reflect.TypeOf(arrow.Date64(0)): arrow.FixedWidthTypes.Date64,
	reflect.TypeOf(true):            arrow.FixedWidthTypes.Boolean,
}

func GetDataType[T NumericTypes | bool | string]() arrow.DataType {
	var z T
	return typMap[reflect.TypeOf(z)]
}

type arrayBuilder[T NumericTypes] interface {
	array.Builder
	Append(T)
	AppendValues([]T, []bool)
}

func ArrayFromSlice[T NumericTypes](mem memory.Allocator, data []T) arrow.Array {
	bldr := array.NewBuilder(mem, typMap[reflect.TypeOf(data).Elem()]).(arrayBuilder[T])
	defer bldr.Release()

	bldr.AppendValues(data, nil)
	return bldr.NewArray()
}

func RechunkArraysConsistently(groups [][]arrow.Array) [][]arrow.Array {
	if len(groups) <= 1 {
		return groups
	}

	var totalLen int
	for _, a := range groups[0] {
		totalLen += a.Len()
	}

	if totalLen == 0 {
		return groups
	}

	rechunked := make([][]arrow.Array, len(groups))
	offsets := make([]int, len(groups))
	// scan all array vectors at once, rechunking along the way
	var start int64
	for start < int64(totalLen) {
		// first compute max possible length for next chunk
		chunkLength := math.MaxInt64
		for i, g := range groups {
			offset := offsets[i]
			// skip any done arrays including 0-length
			for offset == g[0].Len() {
				g = g[1:]
				offset = 0
			}
			arr := g[0]
			chunkLength = Min(chunkLength, arr.Len()-offset)

			offsets[i] = offset
			groups[i] = g
		}

		// now slice all the arrays along this chunk size
		for i, g := range groups {
			offset := offsets[i]
			arr := g[0]
			if offset == 0 && arr.Len() == chunkLength {
				// slice spans entire array
				arr.Retain()
				rechunked[i] = append(rechunked[i], arr)
			} else {
				rechunked[i] = append(rechunked[i], array.NewSlice(arr, int64(offset), int64(offset+chunkLength)))
			}
			offsets[i] += chunkLength
		}

		start += int64(chunkLength)
	}
	return rechunked
}

type ChunkResolver struct {
	offsets []int64
	cached  int64
}

func NewChunkResolver(chunks []arrow.Array) *ChunkResolver {
	offsets := make([]int64, len(chunks)+1)
	var offset int64
	for i, c := range chunks {
		curOffset := offset
		offset += int64(c.Len())
		offsets[i] = curOffset
	}
	offsets[len(chunks)] = offset
	return &ChunkResolver{offsets: offsets}
}

func (c *ChunkResolver) Resolve(idx int64) (chunk, index int64) {
	// some algorithms consecutively access indexes that are a
	// relatively small distance from each other, falling into
	// the same chunk.
	// This is trivial when merging (assuming each side of the
	// merge uses its own resolver), but also in the inner
	// recursive invocations of partitioning.
	if len(c.offsets) <= 1 {
		return 0, idx
	}

	cached := atomic.LoadInt64(&c.cached)
	cacheHit := idx >= c.offsets[cached] && idx < c.offsets[cached+1]
	if cacheHit {
		return cached, idx - c.offsets[cached]
	}

	chkIdx, found := slices.BinarySearch(c.offsets, idx)
	if !found {
		chkIdx--
	}

	chunk, index = int64(chkIdx), idx-c.offsets[chkIdx]
	atomic.StoreInt64(&c.cached, chunk)
	return
}
