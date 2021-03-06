// Author:  Niels A.D.
// Project: gowarcraft3 (https://github.com/nielsAD/gowarcraft3)
// License: Mozilla Public License, v2.0

package w3g

import (
	"bufio"
	"compress/zlib"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"

	"github.com/nielsAD/gowarcraft3/protocol"
)

// Decompressor is an io.Reader that decompresses data blocks
type Decompressor struct {
	RecordDecoder

	SizeRead  uint32 // Compressed size read in total
	SizeTotal uint32 // Decompressed size left to read in total
	SizeBlock uint32 // Decompressed size left to read current block
	NumBlocks uint32 // Blocks left to read

	r   io.Reader
	z   io.ReadCloser
	tee io.Reader
	lim *io.LimitedReader

	crc     hash.Hash32
	crcData uint16
	buf     [12]byte
	bufr    *bufio.Reader
}

// NewDecompressor for compressed w3g data
func NewDecompressor(r io.Reader, e Encoding, f RecordFactory, numBlocks uint32, sizeTotal uint32) *Decompressor {
	var lim = io.LimitedReader{R: r}
	var crc = crc32.NewIEEE()
	var tee = &toByteReader{Reader: io.TeeReader(&lim, crc)}

	return &Decompressor{
		RecordDecoder: RecordDecoder{
			RecordFactory: f,
			Encoding:      e,
		},
		SizeTotal: sizeTotal,
		NumBlocks: numBlocks,
		r:         r,
		tee:       tee,
		lim:       &lim,
		crc:       crc,
	}
}

// For some reason, zlib wants a flate.Reader (io.Reader + io.ByteReader), otherwise
// it implicitly uses a bufio.Reader. Use our own straightforward implementation to
// reduce allocations and prevent reading more than necessary.
type toByteReader struct {
	io.Reader
	b [1]byte
}

// ReadByte implements io.ByteReader interface
func (r *toByteReader) ReadByte() (byte, error) {
	_, err := r.Read(r.b[:])
	return r.b[0], err
}

func (d *Decompressor) nextBlock() error {
	if d.NumBlocks == 0 {
		return io.EOF
	}
	if err := d.closeBlock(); err != nil {
		return err
	}

	d.NumBlocks--

	var lenHead = len(d.buf)
	if d.GameVersion > 0 && d.GameVersion < 10032 {
		lenHead -= 4
	}

	n, err := io.ReadFull(d.r, d.buf[:lenHead])
	d.SizeRead += uint32(n)
	if err != nil {
		return err
	}

	var pbuf = protocol.Buffer{Bytes: d.buf[:lenHead]}
	var lenDeflate uint32
	if d.GameVersion == 0 || d.GameVersion >= 10032 {
		lenDeflate = pbuf.ReadUInt32()
		d.SizeBlock = pbuf.ReadUInt32()
	} else {
		lenDeflate = uint32(pbuf.ReadUInt16())
		d.SizeBlock = uint32(pbuf.ReadUInt16())
	}

	var crcHead = pbuf.ReadUInt16()
	d.crcData = pbuf.ReadUInt16()

	d.buf[lenHead-4], d.buf[lenHead-3], d.buf[lenHead-2], d.buf[lenHead-1] = 0, 0, 0, 0
	var crc = crc32.ChecksumIEEE(d.buf[:lenHead])
	if crcHead != uint16(crc^crc>>16) {
		return ErrInvalidChecksum
	}
	// Use limr to keep track of how many compressed bytes are read
	d.lim.R = d.r
	d.lim.N = int64(lenDeflate)
	d.crc.Reset()

	if d.z == nil {
		d.z, err = zlib.NewReader(d.tee)
	} else {
		err = d.z.(zlib.Resetter).Reset(d.tee, nil)
	}

	// Account for zlib header
	d.SizeRead += lenDeflate - uint32(d.lim.N)

	return err
}

func (d *Decompressor) closeBlock() error {
	if d.SizeBlock > 0 || d.lim.N > 0 {
		return io.ErrUnexpectedEOF
	}

	var sum = d.crc.Sum32()
	if d.crcData != uint16(sum^sum>>16) {
		return ErrInvalidChecksum
	}

	return nil
}

// Read implements the io.Reader interface.
func (d *Decompressor) Read(b []byte) (int, error) {
	if d.SizeTotal == 0 {
		return 0, io.EOF
	}

	var n = 0
	var l = len(b)
	if uint32(l) > d.SizeTotal {
		b = b[:d.SizeTotal]
		l = len(b)
	}

	for n != l {
		if d.SizeBlock == 0 {
			if err := d.nextBlock(); err != nil {
				return n, err
			}
		}

		var r = d.lim.N
		nn, err := io.ReadFull(d.z, b[n:])
		d.SizeRead += uint32(r - d.lim.N)
		d.SizeTotal -= uint32(nn)
		d.SizeBlock -= uint32(nn)
		n += nn

		switch err {
		case nil:
			if d.SizeTotal == 0 && d.SizeBlock > 0 {
				nn, _ := io.Copy(ioutil.Discard, d.z)
				d.SizeBlock -= uint32(nn)
			}
			if d.SizeBlock > 0 {
				continue
			}
			fallthrough
		case io.ErrUnexpectedEOF:
			if err := d.closeBlock(); err != nil {
				return n, err
			}
		default:
			return n, err
		}
	}

	return n, nil
}

// ForEach record call f
func (d *Decompressor) ForEach(f func(r Record) error) error {
	if d.bufr == nil {
		d.bufr = bufio.NewReaderSize(d, 8192)
	}

	for {
		rec, _, err := d.RecordDecoder.Read(d.bufr)
		switch err {
		case nil:
			if err := f(rec); err != nil {
				return err
			}
		case io.EOF:
			return nil
		default:
			return err
		}
	}
}
