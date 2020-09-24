/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package z

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"sort"

	"github.com/pkg/errors"
)

// Buffer is equivalent of bytes.Buffer without the ability to read. It is NOT thread-safe.
//
// In UseCalloc mode, z.Calloc is used to allocate memory, which depending upon how the code is
// compiled could use jemalloc for allocations.
//
// In UseMmap mode, Buffer  uses file mmap to allocate memory. This allows us to store big data
// structures without using physical memory.
//
// MaxSize can be set to limit the memory usage.
type Buffer struct {
	buf     []byte
	offset  int
	curSz   int
	maxSz   int
	fd      *os.File
	bufType BufferType
}

type BufferType int

func (t BufferType) String() string {
	switch t {
	case UseCalloc:
		return "UseCalloc"
	case UseMmap:
		return "UseMmap"
	}
	return "invalid"
}

const (
	UseCalloc BufferType = iota
	UseMmap
	UseInvalid
)

// smallBufferSize is an initial allocation minimal capacity.
const smallBufferSize = 64

// Newbuffer is a helper utility, which creates a virtually unlimited Buffer in UseCalloc mode.
func NewBuffer(sz int) *Buffer {
	buf, err := NewBufferWith(sz, math.MaxInt64, UseCalloc)
	if err != nil {
		log.Fatalf("while creating buffer: %v", err)
	}
	return buf
}

// NewBufferWith would allocate a buffer of size sz upfront, with the total size of the buffer not
// exceeding maxSz. Both sz and maxSz can be set to zero, in which case reasonable defaults would be
// used. Buffer can't be used without initialization via NewBuffer.
func NewBufferWith(sz, maxSz int, bufType BufferType) (*Buffer, error) {
	var buf []byte
	var fd *os.File

	if sz == 0 {
		sz = smallBufferSize
	}
	if maxSz == 0 {
		maxSz = math.MaxInt32
	}

	switch bufType {
	case UseCalloc:
		buf = Calloc(sz)

	case UseMmap:
		var err error
		fd, err = ioutil.TempFile("", "buffer")
		if err != nil {
			return nil, err
		}
		if err := fd.Truncate(int64(sz)); err != nil {
			return nil, errors.Wrapf(err, "while truncating %s to size: %d", fd.Name(), sz)
		}

		buf, err = Mmap(fd, true, int64(maxSz)) // Mmap up to max size.
		if err != nil {
			return nil, errors.Wrapf(err, "while mmapping %s with size: %d", fd.Name(), maxSz)
		}

	default:
		log.Fatalf("Invalid bufType: %q\n", bufType)
	}

	buf[0] = 0x00
	return &Buffer{
		buf:     buf,
		offset:  1, // Always leave offset 0.
		curSz:   sz,
		maxSz:   maxSz,
		fd:      fd,
		bufType: bufType,
	}, nil
}

func NewMmapFile(sz, maxSz, offset int, path string) (*Buffer, error) {
	var buf []byte
	var fd *os.File

	if sz == 0 {
		sz = smallBufferSize
	}
	if maxSz == 0 {
		maxSz = math.MaxInt32
	}

	var err error
	fd, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}

	// If the file already exists and its size is larger than sz, truncate the file to
	// its existing size to avoid losing existing data. Otherwise truncate to the given
	// size.
	fi, err := fd.Stat()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot stat file")
	}
	fileSize := fi.Size()
	if fileSize > int64(maxSz) {
		return nil, errors.Errorf("file size %d is already bigger than max size %d",
			fileSize, maxSz)
	}
	truncateSize := int64(sz)
	if fileSize > truncateSize {
		truncateSize = fileSize
	}
	if err := fd.Truncate(truncateSize); err != nil {
		return nil, errors.Wrapf(err, "while truncating %s to size: %d", fd.Name(), sz)
	}

	buf, err = Mmap(fd, true, int64(maxSz)) // Mmap up to max size.
	if err != nil {
		return nil, errors.Wrapf(err, "while mmapping %s with size: %d", fd.Name(), maxSz)
	}

	// If the file exists, also set the offset to the maximum of fileSize and offset.
	if int(fileSize) > offset {
		offset = int(fileSize)
	}

	return &Buffer{
		buf:     buf,
		offset:  offset,
		curSz:   sz,
		maxSz:   maxSz,
		fd:      fd,
		bufType: UseMmap,
	}, nil
}

// First will return the first n bytes in the buffer.
func (b *Buffer) First(n int) ([]byte, error) {
	if len(b.buf) < n {
		return nil, errors.Errorf("not enough capacity in the buffer to return %d bytes", n)
	}
	return b.buf[0:n], nil
}

func (b *Buffer) IsEmpty() bool {
	return b.offset == 1
}

// Len would return the number of bytes written to the buffer so far.
func (b *Buffer) Len() int {
	return b.offset
}

// Bytes would return all the written bytes as a slice.
func (b *Buffer) Bytes() []byte {
	return b.buf[1:b.offset]
}

// Grow would grow the buffer to have at least n more bytes. In case the buffer is at capacity, it
// would reallocate twice the size of current capacity + n, to ensure n bytes can be written to the
// buffer without further allocation. In UseMmap mode, this might result in underlying file
// expansion.
func (b *Buffer) Grow(n int) {
	// In this case, len and cap are the same.
	if b.buf == nil {
		panic("z.Buffer needs to be initialized before using")
	}
	if b.maxSz-b.offset < n {
		panic(fmt.Sprintf("Buffer max size exceeded: %d."+
			" Offset: %d. Grow: %d", b.maxSz, b.offset, n))
	}
	if b.curSz-b.offset > n {
		return
	}

	growBy := b.curSz + n
	if growBy > 1<<30 {
		growBy = 1 << 30
	}
	if n > growBy {
		// Always at least allocate n, even if it exceeds the 1GB limit above.
		growBy = n
	}
	b.curSz += growBy

	switch b.bufType {
	case UseCalloc:
		newBuf := Calloc(b.curSz)
		copy(newBuf, b.buf[:b.offset])
		Free(b.buf)
		b.buf = newBuf
	case UseMmap:
		if err := b.fd.Truncate(int64(b.curSz)); err != nil {
			log.Fatalf("While trying to truncate file %s to size: %d error: %v\n",
				b.fd.Name(), b.curSz, err)
		}
	}
}

// Allocate is a way to get a slice of size n back from the buffer. This slice can be directly
// written to. Warning: Allocate is not thread-safe. The byte slice returned MUST be used before
// further calls to Buffer.
func (b *Buffer) Allocate(n int) []byte {
	b.Grow(n)
	off := b.offset
	b.offset += n
	return b.buf[off:b.offset]
}

// AllocateOffset works the same way as allocate, but instead of returning a byte slice, it returns
// the offset of the allocation.
func (b *Buffer) AllocateOffset(n int) int {
	b.Grow(n)
	b.offset += n
	return b.offset - n
}

func (b *Buffer) writeLen(sz int) {
	buf := b.Allocate(4)
	binary.BigEndian.PutUint32(buf, uint32(sz))
}

// SliceAllocate would encode the size provided into the buffer, followed by a call to Allocate,
// hence returning the slice of size sz. This can be used to allocate a lot of small buffers into
// this big buffer.
// Note that SliceAllocate should NOT be mixed with normal calls to Write.
func (b *Buffer) SliceAllocate(sz int) []byte {
	b.Grow(4 + sz)
	b.writeLen(sz)
	return b.Allocate(sz)
}

func (b *Buffer) SliceAllocateOffset(sz int) ([]byte, int) {
	b.Grow(4 + sz)
	b.writeLen(sz)
	return b.Allocate(sz), b.offset - sz - 4
}

// Slice would return the slice written at offset.
func (b *Buffer) Slice(offset int) ([]byte, int) {
	if offset >= b.offset {
		return nil, 0
	}

	sz := binary.BigEndian.Uint32(b.buf[offset:])
	start := offset + 4
	next := start + int(sz)
	res := b.buf[start:next]
	if next >= b.offset {
		next = 0
	}
	return res, next
}

func (b *Buffer) Data(offset int) []byte {
	if offset > b.curSz {
		panic("offset beyond current size")
	}
	return b.buf[offset:b.curSz]
}

// Write would write p bytes to the buffer.
func (b *Buffer) Write(p []byte) (n int, err error) {
	b.Grow(len(p))
	n = copy(b.buf[b.offset:], p)
	b.offset += n
	return n, nil
}

func (b *Buffer) WriteAt(p []byte, offset int) (int, error) {
	if offset+len(p) > len(b.buf) {
		return 0, errors.Errorf("cannot write buffer of size %d at offset %d", len(p), offset)
	}

	n := copy(b.buf[offset:], p)
	return n, nil
}

func (b *Buffer) WriteSliceAt(p []byte, offset int) (int, error) {
	if offset+4+len(p) > len(b.buf) {
		return 0, errors.Errorf("cannot write buffer of size %d at offset %d", len(p), offset)
	}

	binary.BigEndian.PutUint32(b.buf[offset:offset+4], uint32(len(p)))
	n := copy(b.buf[offset+4:], p)
	return n, nil
}

func (b *Buffer) ReadAt(n, offset int) ([]byte, error) {
	if offset+n > len(b.buf) {
		return nil, errors.Errorf("cannot %d bytes at offset %d", n, offset)
	}

	return b.buf[offset : offset+n], nil
}

// Reset would reset the buffer to be reused.
func (b *Buffer) Reset() {
	b.offset = 1
}

// Release would free up the memory allocated by the buffer. Once the usage of buffer is done, it is
// important to call Release, otherwise a memory leak can happen.
func (b *Buffer) Release() error {
	switch b.bufType {
	case UseCalloc:
		Free(b.buf)

	case UseMmap:
		fname := b.fd.Name()
		if err := Munmap(b.buf); err != nil {
			return errors.Wrapf(err, "while munmap file %s", fname)
		}
		if err := b.fd.Truncate(0); err != nil {
			return errors.Wrapf(err, "while truncating file %s", fname)
		}
		if err := b.fd.Close(); err != nil {
			return errors.Wrapf(err, "while closing file %s", fname)
		}
		if err := os.Remove(b.fd.Name()); err != nil {
			return errors.Wrapf(err, "while deleting file %s", fname)
		}
	}
	return nil
}

type LessFunc func(a, b []byte) bool
type sortHelper struct {
	offsets []int
	b       *Buffer
	tmp     *Buffer
	less    LessFunc
	small   []int
}

func (s *sortHelper) sortSmall(start, end int) {
	s.tmp.Reset()
	s.small = s.small[:0]
	next := start
	for next != 0 && next < end {
		s.small = append(s.small, next)
		_, next = s.b.Slice(next)
	}

	// We are sorting the slices pointed to by s.small offsets, but only moving the offsets around.
	sort.Slice(s.small, func(i, j int) bool {
		left, _ := s.b.Slice(s.small[i])
		right, _ := s.b.Slice(s.small[j])
		return s.less(left, right)
	})
	// Now we iterate over the s.small offsets and copy over the slices. The result is now in order.
	for _, off := range s.small {
		s.tmp.Write(rawSlice(s.b.buf[off:]))
	}
	assert(end-start == copy(s.b.buf[start:end], s.tmp.Bytes()))
}

func assert(b bool) {
	if !b {
		log.Fatalf("Assertion failure")
	}
}

func check(err error) {
	assert(err == nil)
}

func check2(_ interface{}, err error) {
	check(err)
}

func (s *sortHelper) merge(left, right []byte, start, end int) {
	if len(left) == 0 || len(right) == 0 {
		return
	}
	s.tmp.Reset()
	check2(s.tmp.Write(left))
	left = s.tmp.Bytes()

	var ls, rs []byte

	copyLeft := func() {
		assert(len(ls) == copy(s.b.buf[start:], ls))
		left = left[len(ls):]
		start += len(ls)
	}
	copyRight := func() {
		assert(len(rs) == copy(s.b.buf[start:], rs))
		right = right[len(rs):]
		start += len(rs)
	}

	for start < end {
		if len(left) == 0 {
			assert(len(right) == copy(s.b.buf[start:end], right))
			return
		}
		if len(right) == 0 {
			assert(len(left) == copy(s.b.buf[start:end], left))
			return
		}
		ls = rawSlice(left)
		rs = rawSlice(right)

		// We skip the first 4 bytes in the rawSlice, because that stores the length.
		if s.less(ls[4:], rs[4:]) {
			copyLeft()
		} else {
			copyRight()
		}
	}
}

func (s *sortHelper) sort(lo, hi int) []byte {
	assert(lo <= hi)

	mid := lo + (hi-lo)/2
	loff, hoff := s.offsets[lo], s.offsets[hi]
	if lo == mid {
		// No need to sort, just return the buffer.
		return s.b.buf[loff:hoff]
	}

	// lo, mid would sort from [offset[lo], offset[mid]) .
	left := s.sort(lo, mid)
	// Typically we'd use mid+1, but here mid represents an offset in the buffer. Each offset
	// contains a thousand entries. So, if we do mid+1, we'd skip over those entries.
	right := s.sort(mid, hi)

	s.merge(left, right, loff, hoff)
	return s.b.buf[loff:hoff]
}

// SortSlice is like SortSliceBetween but sorting over the entire buffer.
func (b *Buffer) SortSlice(less func(left, right []byte) bool) {
	b.SortSliceBetween(1, b.offset, less)
}

func (b *Buffer) SortSliceBetween(start, end int, less LessFunc) {
	if start >= end {
		return
	}
	if start == 0 {
		panic("start can never be zero")
	}

	var offsets []int
	next, count := start, 0
	for next != 0 && next < end {
		if count%1024 == 0 {
			offsets = append(offsets, next)
		}
		_, next = b.Slice(next)
		count++
	}
	assert(len(offsets) > 0)
	if offsets[len(offsets)-1] != end {
		offsets = append(offsets, end)
	}

	szTmp := int(float64((end-start)/2) * 1.1)
	s := &sortHelper{
		offsets: offsets,
		b:       b,
		less:    less,
		small:   make([]int, 0, 1024),
		tmp:     NewBuffer(szTmp),
	}
	defer s.tmp.Release()

	left := offsets[0]
	for _, off := range offsets[1:] {
		s.sortSmall(left, off)
		left = off
	}
	s.sort(0, len(offsets)-1)
}

func rawSlice(buf []byte) []byte {
	sz := binary.BigEndian.Uint32(buf)
	return buf[:4+int(sz)]
}
