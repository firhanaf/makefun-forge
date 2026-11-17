package main

// Packfiles and pkt-lines: reading a pushed pack (plain objects, OFS_DELTA and REF_DELTA), writing the
// packs fetches answer with, and the pkt-line framing both protocols share.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// ---------------------------------------------------------------------------------------------
// pkt-line

const (
	pktData = iota
	pktFlush
	pktDelim
	pktEnd
)

type Pkt struct {
	Kind    int
	Payload []byte
}

func (p Pkt) Text() string {
	s := string(p.Payload)
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

// readPkts decodes pkt-lines from data until a stop after `flushes` flush packets (0 = until the end);
// it answers the packets and the bytes after the last one it read.
func readPkts(data []byte, flushes int) ([]Pkt, []byte, error) {
	var out []Pkt
	seen := 0
	for len(data) > 0 {
		if len(data) < 4 {
			return out, data, errors.New("truncated pkt-line length")
		}
		n, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil {
			return out, data, fmt.Errorf("bad pkt-line length %q", data[:4])
		}
		switch n {
		case 0:
			out = append(out, Pkt{Kind: pktFlush})
			data = data[4:]
			seen++
			if flushes > 0 && seen >= flushes {
				return out, data, nil
			}
			continue
		case 1:
			out = append(out, Pkt{Kind: pktDelim})
			data = data[4:]
			continue
		case 2:
			out = append(out, Pkt{Kind: pktEnd})
			data = data[4:]
			continue
		case 3:
			return out, data, errors.New("bad pkt-line length 3")
		}
		if int(n) > len(data) {
			return out, data, errors.New("truncated pkt-line")
		}
		out = append(out, Pkt{Kind: pktData, Payload: data[4:n]})
		data = data[n:]
	}
	return out, data, nil
}

func pktLine(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%04x", len(s)+4)
	b.WriteString(s)
}

func pktRaw(b *bytes.Buffer, p []byte) {
	fmt.Fprintf(b, "%04x", len(p)+4)
	b.Write(p)
}

func pktFlushTo(b *bytes.Buffer) { b.WriteString("0000") }
func pktDelimTo(b *bytes.Buffer) { b.WriteString("0001") }

// sideband writes data on band 1 in chunks no larger than a pkt-line allows.
func sideband(b *bytes.Buffer, band byte, data []byte) {
	const max = 65515
	for len(data) > 0 {
		n := len(data)
		if n > max {
			n = max
		}
		fmt.Fprintf(b, "%04x", n+5)
		b.WriteByte(band)
		b.Write(data[:n])
		data = data[n:]
	}
}

// ---------------------------------------------------------------------------------------------
// pack writing

func writePack(db *ODB, oids []Oid) []byte {
	var b bytes.Buffer
	b.Grow(64 + len(oids)*64)
	b.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(oids)))
	b.Write(hdr[:])
	for _, o := range oids {
		obj := db.Get(o)
		if obj == nil {
			continue
		}
		size := len(obj.Data)
		c := byte(obj.Type<<4) | byte(size&0x0f)
		size >>= 4
		for size > 0 {
			b.WriteByte(c | 0x80)
			c = byte(size & 0x7f)
			size >>= 7
		}
		b.WriteByte(c)
		b.Write(obj.Deflated())
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

// ---------------------------------------------------------------------------------------------
// pack reading

type packEntry struct {
	offset  int
	typ     int
	data    []byte // inflated bytes (the delta itself for a delta entry)
	baseOff int    // OFS_DELTA base offset
	baseOid Oid    // REF_DELTA base id
	oid     Oid
	done    bool
}

// readPack parses a pack and stores every object it resolves; it answers the ids it stored.
func readPack(db *ODB, pack []byte) ([]Oid, error) {
	if len(pack) == 0 {
		return nil, nil
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a pack")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	body := pack[:len(pack)-20]
	if sum := sha1.Sum(body); !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	entries := make([]*packEntry, 0, count)
	byOffset := make(map[int]*packEntry, count)
	pos := 12
	for i := 0; i < count; i++ {
		if pos >= len(body) {
			return nil, errors.New("pack is truncated")
		}
		e := &packEntry{offset: pos}
		c := body[pos]
		pos++
		e.typ = int(c>>4) & 7
		size := int(c & 0x0f)
		shift := 4
		for c&0x80 != 0 {
			if pos >= len(body) {
				return nil, errors.New("pack entry header is truncated")
			}
			c = body[pos]
			pos++
			size |= int(c&0x7f) << shift
			shift += 7
		}
		switch e.typ {
		case 6: // OFS_DELTA
			if pos >= len(body) {
				return nil, errors.New("ofs-delta is truncated")
			}
			c = body[pos]
			pos++
			off := int(c & 0x7f)
			for c&0x80 != 0 {
				c = body[pos]
				pos++
				off = ((off + 1) << 7) | int(c&0x7f)
			}
			e.baseOff = e.offset - off
		case 7: // REF_DELTA
			if pos+20 > len(body) {
				return nil, errors.New("ref-delta is truncated")
			}
			copy(e.baseOid[:], body[pos:pos+20])
			pos += 20
		case objCommit, objTree, objBlob, objTag:
		default:
			return nil, fmt.Errorf("pack entry type %d", e.typ)
		}
		r := bytes.NewReader(body[pos:])
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("zlib: %v", err)
		}
		data := make([]byte, 0, size)
		buf := bytes.NewBuffer(data)
		if _, err := io.Copy(buf, zr); err != nil {
			return nil, fmt.Errorf("inflate: %v", err)
		}
		zr.Close()
		e.data = buf.Bytes()
		pos += len(body[pos:]) - r.Len()
		entries = append(entries, e)
		byOffset[e.offset] = e
	}
	// Resolve plain objects first, then deltas until nothing moves.
	var stored []Oid
	byOid := map[Oid]*packEntry{}
	for _, e := range entries {
		if e.typ >= 1 && e.typ <= 4 {
			e.oid = hashObject(e.typ, e.data)
			e.done = true
			db.PutKnown(e.oid, e.typ, e.data)
			byOid[e.oid] = e
			stored = append(stored, e.oid)
		}
	}
	for progress := true; progress; {
		progress = false
		for _, e := range entries {
			if e.done {
				continue
			}
			var baseType int
			var baseData []byte
			if e.typ == 6 {
				b := byOffset[e.baseOff]
				if b == nil {
					return nil, errors.New("ofs-delta base is not in the pack")
				}
				if !b.done {
					continue
				}
				baseType, baseData = b.typ, b.data
			} else {
				if b := byOid[e.baseOid]; b != nil {
					baseType, baseData = b.typ, b.data
				} else if obj := db.Get(e.baseOid); obj != nil {
					baseType, baseData = obj.Type, obj.Data
				} else {
					continue
				}
			}
			out, err := applyDelta(baseData, e.data)
			if err != nil {
				return nil, err
			}
			e.typ, e.data, e.done = baseType, out, true
			e.oid = hashObject(e.typ, e.data)
			db.PutKnown(e.oid, e.typ, e.data)
			byOid[e.oid] = e
			stored = append(stored, e.oid)
			progress = true
		}
	}
	for _, e := range entries {
		if !e.done {
			return nil, errors.New("a delta's base is missing")
		}
	}
	return stored, nil
}

func deltaVarint(d []byte, pos int) (int, int) {
	v, shift := 0, 0
	for pos < len(d) {
		c := d[pos]
		pos++
		v |= int(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			break
		}
	}
	return v, pos
}

func applyDelta(base, delta []byte) ([]byte, error) {
	srcSize, pos := deltaVarint(delta, 0)
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	dstSize, pos := deltaVarint(delta, pos)
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, n := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<i) != 0 {
					off |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<i) != 0 {
					n |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+n]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert out of range")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}
