// Copyright 2023 StreamNative, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kv

import (
	"github.com/pkg/errors"
	"io"
)

var (
	ErrorKeyNotFound           = errors.New("oxia: key not found")
	MaxSnapshotChunkSize int64 = 1024 * 1024 // bytes

)

type WriteBatch interface {
	io.Closer

	Put(key string, value []byte) error
	Delete(key string) error
	Get(key string) ([]byte, io.Closer, error)

	DeleteRange(lowerBound, upperBound string) error
	KeyRangeScan(lowerBound, upperBound string) KeyIterator

	// Count is the number of transactions that are currently in the batch
	Count() int

	// Size of all the transactions that are currently in the batch
	Size() int

	Commit() error
}

type KeyIterator interface {
	io.Closer

	Valid() bool
	Key() string
	Next() bool
}

type ReverseKeyIterator interface {
	io.Closer

	Valid() bool
	Key() string
	Prev() bool
}

type KeyValueIterator interface {
	KeyIterator

	Value() ([]byte, error)
}

type SnapshotChunk interface {
	Name() string
	Index() int32
	TotalCount() int32
	Content() []byte
}

type Snapshot interface {
	io.Closer

	BasePath() string

	Valid() bool
	Chunk() (SnapshotChunk, error)
	Next() bool
}

type SnapshotLoader interface {
	io.Closer

	AddChunk(fileName string, chunkIndex int32, chunkCount int32, content []byte) error

	// Complete signals that the snapshot is now complete
	Complete()
}

type KV interface {
	io.Closer

	NewWriteBatch() WriteBatch

	Get(key string) ([]byte, io.Closer, error)

	KeyRangeScan(lowerBound, upperBound string) KeyIterator
	KeyRangeScanReverse(lowerBound, upperBound string) ReverseKeyIterator

	RangeScan(lowerBound, upperBound string) KeyValueIterator

	Snapshot() (Snapshot, error)

	Flush() error

	Delete() error
}

type KVFactoryOptions struct {
	DataDir   string
	CacheSize int64

	// Create a pure in-memory database. Used for unit-tests
	InMemory bool
}

var DefaultKVFactoryOptions = &KVFactoryOptions{
	DataDir:   "data",
	CacheSize: 100 * 1024 * 1024,
	InMemory:  false,
}

type KVFactory interface {
	io.Closer

	NewKV(namespace string, shardId int64) (KV, error)

	NewSnapshotLoader(namespace string, shardId int64) (SnapshotLoader, error)
}
