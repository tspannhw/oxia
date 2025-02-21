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

/*
The MIT License (MIT)

Copyright (c) 2022 streamnative.io
Copyright (c) 2019 Joshua J Baker

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/spf13/afero"
	"github.com/tidwall/tinylru"
	"os"
	"oxia/common"
	"oxia/common/metrics"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	startSuffix           = ".START"
	endSuffix             = ".END"
	truncateSuffix        = ".TRUNCATE"
	tempFileName          = "TEMP"
	segmentFilenameLength = 20
)

var (
	// ErrCorrupt is returns when the log is corrupt.
	ErrCorrupt = errors.New("log corrupt")

	// ErrClosed is returned when an operation cannot be completed because
	// the log is closed.
	ErrClosed = errors.New("log closed")

	// ErrNotFound is returned when an entry is not found.
	ErrNotFound = errors.New("not found")

	// ErrOutOfRange is returned from TruncateFront() and TruncateBack() when
	// the offset is not in the range of the log's first and last offset.
	ErrOutOfRange = errors.New("out of range")
)

// LogFormat is the format of the log files.
type LogFormat byte

// Options for Log
type Options struct {
	// NoSync disables fsync after writes. This is less durable and puts the
	// log at risk of data loss when there's a server crash.
	NoSync bool
	// SegmentSize of each segment. This is just a target value, actual size
	// may differ. Default is 20 MB.
	SegmentSize int
	// SegmentCacheSize is the maximum number of segments that will be held in
	// memory for caching. Increasing this value may enhance performance for
	// concurrent read operations. Default is 1
	SegmentCacheSize int
	// NoCopy allows for the Read() operation to return the raw underlying data
	// slice. This is an optimization to help minimize allocations. When this
	// option is set, do not modify the returned data because it may affect
	// other Read calls. Default false
	NoCopy bool
	// Perms represents the datafiles modes and permission bits
	DirPerms  os.FileMode
	FilePerms os.FileMode

	InMemory bool
}

// DefaultOptions for open().
func DefaultOptions() *Options {
	return &Options{
		NoSync:           false,    // Fsync after every write
		SegmentSize:      20971520, // 20 MB log segment files.
		SegmentCacheSize: 2,        // Number of cached in-memory segments
		NoCopy:           true,     // Make a new copy of data for every Read call.
		DirPerms:         0750,     // Permissions for the created directories
		FilePerms:        0640,     // Permissions for the created data files
		InMemory:         false,
	}
}

// Log represents a write-ahead log
type Log struct {
	mu          sync.RWMutex
	path        string      // absolute path to log directory
	opts        Options     // log options
	closed      bool        // log is closed
	corrupt     bool        // log may be corrupt
	segments    []*segment  // all known log segments
	firstOffset int64       // offset of the first entry in log
	lastOffset  int64       // offset of the last entry in log
	sfile       afero.File  // tail segment file handle
	wbatch      Batch       // reusable write batch
	scache      tinylru.LRU // segment entries cache
	fs          afero.Fs    // Filesystem

	syncLatency metrics.LatencyHistogram
}

// segment represents a single segment file.
type segment struct {
	path   string // path of segment file
	offset int64  // first offset of segment
	ebuf   []byte // cached entries buffer
	epos   []bpos // cached entries positions in buffer
}

type bpos struct {
	pos int // byte position
	end int // one byte past pos
}

func open(path string, opts *Options) (*Log, error) {
	return OpenWithShard(path, common.DefaultNamespace, 0, opts)
}

// OpenWithShard a new write-ahead log
func OpenWithShard(path string, namespace string, shard int64, opts *Options) (*Log, error) {
	defaultOptions := DefaultOptions()
	if opts == nil {
		opts = defaultOptions
	}
	if opts.SegmentCacheSize <= 0 {
		opts.SegmentCacheSize = defaultOptions.SegmentCacheSize
	}
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = defaultOptions.SegmentSize
	}
	if opts.DirPerms == 0 {
		opts.DirPerms = defaultOptions.DirPerms
	}
	if opts.FilePerms == 0 {
		opts.FilePerms = defaultOptions.FilePerms
	}
	fs := afero.NewOsFs()
	if opts.InMemory {
		fs = afero.NewMemMapFs()
	}
	var err error
	path, err = abs(fs, path)
	if err != nil {
		return nil, err
	}
	l := &Log{
		path: path,
		opts: *opts,
		fs:   fs,
		syncLatency: metrics.NewLatencyHistogram("oxia_server_wal_sync_latency",
			"The time it takes to fsync the wal data on disk",
			metrics.LabelsForShard(namespace, shard)),
	}
	l.scache.Resize(l.opts.SegmentCacheSize)
	if err := l.fs.MkdirAll(path, l.opts.DirPerms); err != nil {
		return nil, err
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func abs(fs afero.Fs, path string) (string, error) {

	if _, os := fs.(afero.OsFs); os {
		return filepath.Abs(path)
	}
	return filepath.Clean(path), nil
}

func (l *Log) pushCache(segIdx int) {
	_, _, _, v, evicted :=
		l.scache.SetEvicted(segIdx, l.segments[segIdx])
	if evicted {
		s := v.(*segment)
		s.ebuf = nil
		s.epos = nil
	}
}

// load all the segments. This operation also cleans up any START/END segments.
func (l *Log) load() error {
	fis, err := afero.ReadDir(l.fs, l.path)
	if err != nil {
		return err
	}
	startIdx := -1
	endIdx := -1
	truncateIdx := -1
	for _, fi := range fis {
		name := fi.Name()
		if fi.IsDir() || len(name) < segmentFilenameLength {
			continue
		}
		index, err := strconv.ParseInt(name[:segmentFilenameLength], 10, 64)
		if err != nil || index == -1 {
			continue
		}
		isStart := len(name) == segmentFilenameLength+len(startSuffix) && strings.HasSuffix(name, startSuffix)
		isEnd := len(name) == segmentFilenameLength+len(endSuffix) && strings.HasSuffix(name, endSuffix)
		isTruncate := len(name) == segmentFilenameLength+len(truncateSuffix) && strings.HasSuffix(name, truncateSuffix)
		if len(name) == segmentFilenameLength || isStart || isEnd || isTruncate {
			if isStart {
				startIdx = len(l.segments)
			} else if isEnd && endIdx == -1 {
				endIdx = len(l.segments)
			} else if isTruncate {
				if truncateIdx != -1 {
					return ErrCorrupt
				}
				truncateIdx = len(l.segments)
			}
			l.segments = append(l.segments, &segment{
				offset: index,
				path:   filepath.Join(l.path, name),
			})
		}
	}
	if len(l.segments) == 0 {
		// Create a new log
		return l.createInitialSegment(0)
	}
	if (startIdx != -1 && endIdx != -1) || (startIdx != -1 && truncateIdx != -1) || (truncateIdx != -1 && endIdx != -1) {
		return ErrCorrupt
	}
	// open existing log. Clean up log if START or END segments exists.
	if startIdx != -1 {
		// Delete all files leading up to START
		for i := 0; i < startIdx; i++ {
			if err := l.fs.Remove(l.segments[i].path); err != nil {
				return err
			}
		}
		l.segments = append([]*segment{}, l.segments[startIdx:]...)
		// Rename the START segment
		orgPath := l.segments[0].path
		finalPath := orgPath[:len(orgPath)-len(startSuffix)]
		err := l.fs.Rename(orgPath, finalPath)
		if err != nil {
			return err
		}
		l.segments[0].path = finalPath
	}
	if endIdx != -1 {
		// Delete all files following END
		for i := len(l.segments) - 1; i > endIdx; i-- {
			if err := l.fs.Remove(l.segments[i].path); err != nil {
				return err
			}
		}
		l.segments = append([]*segment{}, l.segments[:endIdx+1]...)
		if len(l.segments) > 1 && l.segments[len(l.segments)-2].offset ==
			l.segments[len(l.segments)-1].offset {
			// remove the segment prior to the END segment because it shares
			// the same starting offset.
			l.segments[len(l.segments)-2] = l.segments[len(l.segments)-1]
			l.segments = l.segments[:len(l.segments)-1]
		}
		// Rename the END segment
		orgPath := l.segments[len(l.segments)-1].path
		finalPath := orgPath[:len(orgPath)-len(endSuffix)]
		err := l.fs.Rename(orgPath, finalPath)
		if err != nil {
			return err
		}
		l.segments[len(l.segments)-1].path = finalPath
	}
	if truncateIdx != -1 {
		// Delete all files other than TRUNCATE
		for i := 0; i < len(l.segments); i++ {
			if i == truncateIdx {
				continue
			}
			if err := l.fs.Remove(l.segments[i].path); err != nil {
				return err
			}
		}
		l.segments = append([]*segment{}, l.segments[truncateIdx])
		// Rename the TRUNCATE segment
		orgPath := l.segments[0].path
		finalPath := orgPath[:len(orgPath)-len(truncateSuffix)]
		err := l.fs.Rename(orgPath, finalPath)
		if err != nil {
			return err
		}
		l.segments[0].path = finalPath
	}
	l.firstOffset = l.segments[0].offset
	// open the last segment for appending
	lseg := l.segments[len(l.segments)-1]
	l.sfile, err = l.openFile(lseg.path)
	if err != nil {
		return err
	}
	if _, err := l.sfile.Seek(0, 2); err != nil {
		return err
	}
	// Load the last segment entries
	if err := l.loadSegmentEntries(lseg); err != nil {
		return err
	}
	l.lastOffset = lseg.offset + int64(len(lseg.epos)) - 1
	return nil
}

// segmentName returns a 20-byte textual representation of an offset
// for lexical ordering. This is used for the file names of log segments.
func segmentName(offset int64) string {
	return fmt.Sprintf("%020d", offset)
}

func (l *Log) createInitialSegment(offset int64) error {
	l.segments = append(l.segments, &segment{
		offset: offset,
		path:   filepath.Join(l.path, segmentName(offset)),
	})
	l.firstOffset = offset
	l.lastOffset = offset - 1

	var err error
	l.sfile, err = l.newFile(l.segments[0].path)
	return err
}

// Close the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		if l.corrupt {
			return ErrCorrupt
		}
		return ErrClosed
	}
	if err := l.syncNoMutex(); err != nil {
		return err
	}
	if err := l.sfile.Close(); err != nil {
		return err
	}
	l.closed = true
	if l.corrupt {
		return ErrCorrupt
	}
	return nil
}

// Write an entry to the log.
func (l *Log) Write(offset int64, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}
	l.wbatch.Clear()
	l.wbatch.Write(offset, data)
	return l.writeBatch(&l.wbatch)
}

// Cycle the old segment for a new segment.
func (l *Log) cycle(nextOffset int64) error {
	if err := l.syncNoMutex(); err != nil {
		return err
	}
	if err := l.sfile.Close(); err != nil {
		return err
	}

	lastSegment := l.segments[len(l.segments)-1]
	if lastSegment.offset == 0 && len(lastSegment.ebuf) == 0 {
		// We're removing an initial empty segment, because we're
		// jumping to a new offset
		l.firstOffset = nextOffset
		if err := l.fs.Remove(lastSegment.path); err != nil {
			return err
		}
	} else {
		// cache the previous segment
		l.pushCache(len(l.segments) - 1)
	}

	s := &segment{
		offset: nextOffset,
		path:   filepath.Join(l.path, segmentName(nextOffset)),
	}
	var err error
	l.sfile, err = l.newFile(s.path)
	if err != nil {
		return err
	}
	l.segments = append(l.segments, s)
	return nil
}

func appendEntry(dst []byte, data []byte) (out []byte, epos bpos) {
	// data_size + data
	pos := len(dst)
	dst = appendUvarint(dst, uint64(len(data)))
	dst = append(dst, data...)
	return dst, bpos{pos, len(dst)}
}

func appendUvarint(dst []byte, x uint64) []byte {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], x)
	dst = append(dst, buf[:n]...)
	return dst
}

// Batch of entries. Used to write multiple entries at once using WriteBatch().
type Batch struct {
	entries []batchEntry
	datas   []byte
}

type batchEntry struct {
	offset int64
	size   int
}

// Write an entry to the batch
func (b *Batch) Write(offset int64, data []byte) {
	b.entries = append(b.entries, batchEntry{offset: offset, size: len(data)})
	b.datas = append(b.datas, data...)
}

// Clear the batch for reuse.
func (b *Batch) Clear() {
	b.entries = b.entries[:0]
	b.datas = b.datas[:0]
}

// WriteBatch writes the entries in the batch to the log in the order that they
// were added to the batch. The batch is cleared upon a successful return.
func (l *Log) WriteBatch(b *Batch) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}
	if len(b.entries) == 0 {
		return nil
	}
	return l.writeBatch(b)
}

func (l *Log) writeBatch(b *Batch) error {
	// load the tail segment
	s := l.segments[len(l.segments)-1]
	firstOffsetInBatch := b.entries[0].offset
	if firstOffsetInBatch > l.lastOffset+1 ||
		len(s.ebuf) > l.opts.SegmentSize {
		// tail segment has reached capacity. Close it and create a new one.
		if err := l.cycle(firstOffsetInBatch); err != nil {
			return err
		}
		s = l.segments[len(l.segments)-1]
	}

	mark := len(s.ebuf)
	datas := b.datas
	for i := 0; i < len(b.entries); i++ {
		data := datas[:b.entries[i].size]
		var epos bpos
		s.ebuf, epos = appendEntry(s.ebuf, data)
		s.epos = append(s.epos, epos)
		if len(s.ebuf) >= l.opts.SegmentSize {
			// segment has reached capacity, cycle now
			if _, err := l.sfile.Write(s.ebuf[mark:]); err != nil {
				return err
			}
			l.lastOffset = b.entries[i].offset
			if err := l.cycle(b.entries[i].offset + 1); err != nil {
				return err
			}
			s = l.segments[len(l.segments)-1]
			mark = 0
		}
		datas = datas[b.entries[i].size:]
	}
	if len(s.ebuf)-mark > 0 {
		if _, err := l.sfile.Write(s.ebuf[mark:]); err != nil {
			return err
		}
		l.lastOffset = b.entries[len(b.entries)-1].offset
	}
	if !l.opts.NoSync {
		if err := l.syncNoMutex(); err != nil {
			return err
		}
	}
	b.Clear()
	return nil
}

// FirstIndex returns the offset of the first entry in the log. Returns -1
// when log has no entries.
func (l *Log) FirstIndex() (index int64, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.corrupt {
		return 0, ErrCorrupt
	} else if l.closed {
		return 0, ErrClosed
	}
	return l.firstOffset, nil
}

// LastIndex returns the offset of the last entry in the log. Returns zero when
// log has no entries.
func (l *Log) LastIndex() (index int64, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.corrupt {
		return 0, ErrCorrupt
	} else if l.closed {
		return 0, ErrClosed
	}
	return l.lastOffset, nil
}

// findSegment performs a bsearch on the segments
func (l *Log) findSegment(index int64) int {
	i, j := 0, len(l.segments)
	for i < j {
		h := i + (j-i)/2
		if index >= l.segments[h].offset {
			i = h + 1
		} else {
			j = h
		}
	}
	return i - 1
}

func (l *Log) loadSegmentEntries(s *segment) error {
	data, err := afero.ReadFile(l.fs, s.path)
	if err != nil {
		return err
	}
	ebuf := data
	var epos []bpos
	var pos int
	for exidx := s.offset; len(data) > 0; exidx++ {
		n, err := loadNextEntry(data)
		if err != nil {
			return err
		}
		data = data[n:]
		epos = append(epos, bpos{pos, pos + n})
		pos += n
	}
	s.ebuf = ebuf
	s.epos = epos
	return nil
}

func loadNextEntry(data []byte) (n int, err error) {
	// data_size + data
	size, n := binary.Uvarint(data)
	if n <= 0 {
		return 0, ErrCorrupt
	}
	if uint64(len(data)-n) < size {
		return 0, ErrCorrupt
	}
	return n + int(size), nil
}

// loadSegment loads the segment entries into memory, pushes it to the front
// of the lru cache, and returns it.
func (l *Log) loadSegment(index int64) (*segment, error) {
	// check the last segment first.
	lseg := l.segments[len(l.segments)-1]
	if index >= lseg.offset {
		return lseg, nil
	}
	// check the most recent cached segment
	var rseg *segment
	l.scache.Range(func(_, v interface{}) bool {
		s := v.(*segment)
		if index >= s.offset && index < s.offset+int64(len(s.epos)) {
			rseg = s
		}
		return false
	})
	if rseg != nil {
		return rseg, nil
	}
	// find in the segment array
	idx := l.findSegment(index)
	s := l.segments[idx]
	if len(s.epos) == 0 {
		// load the entries from cache
		if err := l.loadSegmentEntries(s); err != nil {
			return nil, err
		}
	}
	// push the segment to the front of the cache
	l.pushCache(idx)
	return s, nil
}

// Read an entry from the log. Returns a byte slice containing the data entry.
func (l *Log) Read(offset int64) (data []byte, err error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.corrupt {
		return nil, ErrCorrupt
	} else if l.closed {
		return nil, ErrClosed
	}
	if offset < l.firstOffset || offset > l.lastOffset {
		return nil, ErrNotFound
	}
	s, err := l.loadSegment(offset)
	if err != nil {
		return nil, err
	}

	epos := s.epos[offset-s.offset]
	edata := s.ebuf[epos.pos:epos.end]

	size, n := binary.Uvarint(edata)
	if n <= 0 {
		return nil, ErrCorrupt
	}
	if uint64(len(edata)-n) < size {
		return nil, ErrCorrupt
	}
	if l.opts.NoCopy {
		data = edata[n : uint64(n)+size]
	} else {
		data = make([]byte, size)
		copy(data, edata[n:])
	}
	return data, nil
}

// ClearCache clears the segment cache
func (l *Log) ClearCache() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}
	l.clearCache()
	return nil
}
func (l *Log) clearCache() {
	l.scache.Range(func(_, v interface{}) bool {
		s := v.(*segment)
		s.ebuf = nil
		s.epos = nil
		return true
	})
	l.scache = tinylru.LRU{}
	l.scache.Resize(l.opts.SegmentCacheSize)
}

// TruncateFront trims the front of the log by removing all entries that
// are before the provided `offset`. In other words the entry at
// `offset` becomes the first entry in the log.
func (l *Log) TruncateFront(index int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}
	return l.truncateFront(index)
}
func (l *Log) truncateFront(index int64) (err error) {
	if index < l.firstOffset || index > l.lastOffset {
		return ErrOutOfRange
	}
	if index == l.firstOffset {
		// nothing to truncate
		return nil
	}
	segIdx := l.findSegment(index)
	var s *segment
	s, err = l.loadSegment(index)
	if err != nil {
		return err
	}
	epos := s.epos[index-s.offset:]
	ebuf := s.ebuf[epos[0].pos:]
	// Create a temp file contains the truncated segment.
	tempName := filepath.Join(l.path, tempFileName)
	err = func() error {
		f, err := l.newFile(tempName)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(ebuf); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return f.Close()
	}()
	// Rename the TEMP file to its START file name.
	startName := filepath.Join(l.path, segmentName(index)+startSuffix)
	if err = l.fs.Rename(tempName, startName); err != nil {
		return err
	}
	// The log was truncated but still needs some file cleanup. Any errors
	// following this message will not cause an on-disk data corruption, but
	// may cause an inconsistency with the current program, so we'll return
	// ErrCorrupt so the user can attempt a recovery by calling Close()
	// followed by open().
	defer func() {
		if v := recover(); v != nil {
			err = ErrCorrupt
			l.corrupt = true
		}
	}()
	if segIdx == len(l.segments)-1 {
		// Close the tail segment file
		if err = l.sfile.Close(); err != nil {
			return err
		}
	}
	// Delete truncated segment files
	for i := 0; i <= segIdx; i++ {
		if err = l.fs.Remove(l.segments[i].path); err != nil {
			return err
		}
	}
	// Rename the START file to the final truncated segment name.
	newName := filepath.Join(l.path, segmentName(index))
	if err = l.fs.Rename(startName, newName); err != nil {
		return err
	}
	s.path = newName
	s.offset = index
	if segIdx == len(l.segments)-1 {
		// Reopen the tail segment file
		if l.sfile, err = l.openFile(newName); err != nil {
			return err
		}
		var n int64
		if n, err = l.sfile.Seek(0, 2); err != nil {
			return err
		}
		if n != int64(len(ebuf)) {
			err = errors.New("invalid seek")
			return err
		}
		// Load the last segment entries
		if err = l.loadSegmentEntries(s); err != nil {
			return err
		}
	}
	l.segments = append([]*segment{}, l.segments[segIdx:]...)
	l.firstOffset = index
	l.clearCache()
	return nil
}

func (l *Log) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}

	return l.clear()
}

func (l *Log) clear() error {
	l.clearCache()

	for _, s := range l.segments {
		os.Remove(s.path)
	}

	l.segments = make([]*segment, 0)
	return l.createInitialSegment(0)
}

// TruncateBack truncates the back of the log by removing all entries that
// are after the provided `offset`. In other words the entry at `offset`
// becomes the last entry in the log.
func (l *Log) TruncateBack(index int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}
	return l.truncateBack(index)
}

func (l *Log) truncateBack(index int64) (err error) {
	if index == l.firstOffset-1 {
		return l.truncateBackAll(l.firstOffset)
	}
	if index < l.firstOffset || index > l.lastOffset {
		return ErrOutOfRange
	}
	if index == l.lastOffset {
		// nothing to truncate
		return nil
	}
	segIdx := l.findSegment(index)
	var s *segment
	s, err = l.loadSegment(index)
	if err != nil {
		return err
	}
	epos := s.epos[:index-s.offset+1]
	ebuf := s.ebuf[:epos[len(epos)-1].end]
	// Create a temp file contains the truncated segment.
	tempName := filepath.Join(l.path, tempFileName)
	err = func() error {
		f, err := l.newFile(tempName)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(ebuf); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		return f.Close()
	}()
	// Rename the TEMP file to its END file name.
	endName := filepath.Join(l.path, segmentName(s.offset)+endSuffix)
	if err = l.fs.Rename(tempName, endName); err != nil {
		return err
	}
	// The log was truncated but still needs some file cleanup. Any errors
	// following this message will not cause an on-disk data corruption, but
	// may cause an inconsistency with the current program, so we'll return
	// ErrCorrupt so the user can attempt a recover by calling Close()
	// followed by open().
	defer func() {
		if v := recover(); v != nil {
			err = ErrCorrupt
			l.corrupt = true
		}
	}()

	// Close the tail segment file
	if err = l.sfile.Close(); err != nil {
		return err
	}
	// Delete truncated segment files
	for i := segIdx; i < len(l.segments); i++ {
		if err = l.fs.Remove(l.segments[i].path); err != nil {
			return err
		}
	}
	// Rename the END file to the final truncated segment name.
	newName := filepath.Join(l.path, segmentName(s.offset))
	if err = l.fs.Rename(endName, newName); err != nil {
		return err
	}
	// Reopen the tail segment file
	if l.sfile, err = l.openFile(newName); err != nil {
		return err
	}
	var n int64
	n, err = l.sfile.Seek(0, 2)
	if err != nil {
		return err
	}
	if n != int64(len(ebuf)) {
		err = errors.New("invalid seek")
		return err
	}
	s.path = newName
	l.segments = append([]*segment{}, l.segments[:segIdx+1]...)
	l.lastOffset = index
	l.clearCache()
	if err = l.loadSegmentEntries(s); err != nil {
		return err
	}
	return nil
}

func (l *Log) truncateBackAll(newFirstIndex int64) (err error) {
	if newFirstIndex == l.lastOffset {
		// nothing to truncate
		return nil
	}

	// Create a temp file that contains the truncated segment.
	tempName := filepath.Join(l.path, segmentName(newFirstIndex)+truncateSuffix)
	f, err := l.openFile(tempName)
	if err != nil {
		return err
	}
	err = f.Close()
	if err != nil {
		return err
	}

	// The log was truncated but still needs some file cleanup. Any errors
	// following this message will not cause an on-disk data corruption, but
	// may cause an inconsistency with the current program, so we'll return
	// ErrCorrupt so the user can attempt a recover by calling Close()
	// followed by open().
	defer func() {
		if v := recover(); v != nil {
			err = ErrCorrupt
			l.corrupt = true
		}
	}()

	// Close the tail segment file
	if err = l.sfile.Close(); err != nil {
		return err
	}
	// Delete all segment files
	for i := 0; i < len(l.segments); i++ {
		if err = l.fs.Remove(l.segments[i].path); err != nil {
			return err
		}
	}
	// Rename the TRUNCATE file to the final truncated segment name.
	newName := filepath.Join(l.path, segmentName(newFirstIndex))
	if err = l.fs.Rename(tempName, newName); err != nil {
		return err
	}
	// Reopen the tail segment file
	if l.sfile, err = l.openFile(newName); err != nil {
		return err
	}

	l.segments = append([]*segment{}, &segment{
		path:   newName,
		offset: newFirstIndex,
	})
	l.lastOffset = newFirstIndex - 1
	l.clearCache()
	return l.loadSegmentEntries(l.segments[0])
}

// Sync performs an fsync on the log. This is not necessary when the
// NoSync option is set to false.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.corrupt {
		return ErrCorrupt
	} else if l.closed {
		return ErrClosed
	}
	return l.syncNoMutex()
}

func (l *Log) syncNoMutex() error {
	timer := l.syncLatency.Timer()
	defer timer.Done()

	return doFSync(l.sfile)
}

func (l *Log) newFile(name string) (afero.File, error) {
	return l.fs.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, l.opts.FilePerms)
}

func (l *Log) openFile(name string) (afero.File, error) {
	return l.fs.OpenFile(name, os.O_WRONLY, l.opts.FilePerms)
}
