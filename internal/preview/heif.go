package preview

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// A HEIC photograph from a phone holds two pictures: the one that was taken,
// and a small one the camera saved beside it. On a recent iPhone the first is
// forty-eight megapixels spread over fifty-four separately coded tiles, and the
// second is a single frame of about thirty kilobytes.
//
// Mirage serves the small one, and does not decode anything to do it. The
// thumbnail is lifted out with its own decoder configuration and wrapped in a
// minimal container, which is a valid HEIC file in its own right - so a device
// that can read HEIC at all, which is every iPhone, decodes it in hardware for
// free. Measured on a real photograph: thirty-two kilobytes instead of ten
// megabytes, and no decoder on the server.
//
// Decoding the full image was never a realistic alternative. It took nearly
// seven seconds with an optimised C decoder on a fast laptop; on a NAS, through
// the only decoder a cgo-free build can use, it would be minutes per picture.

const (
	// maxMetaSize bounds the metadata read from a file. It is a few tens of
	// kilobytes in practice; the bound exists because the size is read from the
	// file itself.
	maxMetaSize = 8 << 20
	// maxThumbnailSize bounds the extracted picture. A camera thumbnail is tens
	// of kilobytes, so anything far larger is not one.
	maxThumbnailSize = 4 << 20
	// maxItems bounds how many entries a file may declare.
	maxItems = 4096
)

// ErrNoThumbnail is returned for a HEIF file that carries no small copy. There
// is nothing to be done about it here: the alternative is decoding the full
// picture, which is what this exists to avoid.
var ErrNoThumbnail = errors.New("this file has no embedded thumbnail")

// heifThumbnail is a camera's own small copy, as a standalone HEIC file.
type heifThumbnail struct {
	Data          []byte
	Width, Height int
}

// box is one ISO base media file format box.
type box struct {
	Type           string
	Start, End     int // the payload, excluding the header
	BoxStart, End2 int
}

// parseBoxes reads the boxes laid out in a range.
func parseBoxes(buf []byte, start, end int) []box {
	var out []box
	for i := start; i+8 <= end; {
		size := int(binary.BigEndian.Uint32(buf[i : i+4]))
		typ := string(buf[i+4 : i+8])
		hdr := 8
		switch {
		case size == 1:
			if i+16 > end {
				return out
			}
			big := binary.BigEndian.Uint64(buf[i+8 : i+16])
			if big > uint64(end-i) {
				return out
			}
			size, hdr = int(big), 16
		case size == 0:
			size = end - i
		}
		if size < hdr || i+size > end {
			return out
		}
		out = append(out, box{Type: typ, Start: i + hdr, End: i + size, BoxStart: i, End2: i + size})
		i += size
	}
	return out
}

// child finds one box by type within a range.
func child(buf []byte, start, end int, typ string) (box, bool) {
	for _, b := range parseBoxes(buf, start, end) {
		if b.Type == typ {
			return b, true
		}
	}
	return box{}, false
}

// extractHEIFThumbnail lifts a camera's small copy out of a HEIF file.
func extractHEIFThumbnail(r io.ReadSeeker) (heifThumbnail, error) {
	meta, metaStart, metaEnd, err := readMeta(r)
	if err != nil {
		return heifThumbnail{}, err
	}

	primary, err := primaryItem(meta, metaStart, metaEnd)
	if err != nil {
		return heifThumbnail{}, err
	}
	thumbID, err := thumbnailItem(meta, metaStart, metaEnd, primary)
	if err != nil {
		return heifThumbnail{}, err
	}

	props, err := itemProperties(meta, metaStart, metaEnd, thumbID)
	if err != nil {
		return heifThumbnail{}, err
	}
	extents, err := itemExtents(meta, metaStart, metaEnd, thumbID)
	if err != nil {
		return heifThumbnail{}, err
	}

	payload, err := readExtents(r, extents)
	if err != nil {
		return heifThumbnail{}, err
	}

	data, err := buildHEIF(props, payload)
	if err != nil {
		return heifThumbnail{}, err
	}
	return heifThumbnail{Data: data, Width: props.Width, Height: props.Height}, nil
}

// readMeta reads the file's metadata box, which holds everything except the
// picture data itself.
func readMeta(r io.ReadSeeker) (buf []byte, start, end int, err error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	// The header of each top-level box is read first so that only the metadata
	// is loaded, rather than the whole photograph.
	var offset int64
	for range 16 {
		var head [16]byte
		if _, err := io.ReadFull(r, head[:8]); err != nil {
			return nil, 0, 0, ErrNoThumbnail
		}
		size := int64(binary.BigEndian.Uint32(head[:4]))
		typ := string(head[4:8])
		hdr := int64(8)
		if size == 1 {
			if _, err := io.ReadFull(r, head[8:16]); err != nil {
				return nil, 0, 0, ErrNoThumbnail
			}
			size, hdr = int64(binary.BigEndian.Uint64(head[8:16])), 16
		}
		if size < hdr {
			return nil, 0, 0, ErrNoThumbnail
		}

		if typ == "meta" {
			if size > maxMetaSize {
				return nil, 0, 0, fmt.Errorf("metadata of %d bytes is larger than this reads", size)
			}
			body := make([]byte, size-hdr)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, 0, 0, ErrNoThumbnail
			}
			// meta is a full box: a version and flags precede its children.
			if len(body) < 4 {
				return nil, 0, 0, ErrNoThumbnail
			}
			return body, 4, len(body), nil
		}

		offset += size
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return nil, 0, 0, ErrNoThumbnail
		}
	}
	return nil, 0, 0, ErrNoThumbnail
}

// primaryItem returns the id of the picture the file is of.
func primaryItem(buf []byte, start, end int) (uint32, error) {
	b, ok := child(buf, start, end, "pitm")
	if !ok || b.End-b.Start < 6 {
		return 0, ErrNoThumbnail
	}
	version := buf[b.Start]
	p := b.Start + 4
	if version == 0 {
		return uint32(binary.BigEndian.Uint16(buf[p : p+2])), nil
	}
	if b.End-p < 4 {
		return 0, ErrNoThumbnail
	}
	return binary.BigEndian.Uint32(buf[p : p+4]), nil
}

// thumbnailItem finds the item that declares itself a thumbnail of the primary.
func thumbnailItem(buf []byte, start, end int, primary uint32) (uint32, error) {
	iref, ok := child(buf, start, end, "iref")
	if !ok || iref.End-iref.Start < 4 {
		return 0, ErrNoThumbnail
	}
	version := buf[iref.Start]
	idSize := 2
	if version != 0 {
		idSize = 4
	}

	for _, ref := range parseBoxes(buf, iref.Start+4, iref.End) {
		if ref.Type != "thmb" {
			continue
		}
		p := ref.Start
		if p+idSize+2 > ref.End {
			continue
		}
		from := readUint(buf, p, idSize)
		p += idSize
		count := int(binary.BigEndian.Uint16(buf[p : p+2]))
		p += 2
		for range count {
			if p+idSize > ref.End {
				break
			}
			if readUint(buf, p, idSize) == primary {
				return from, nil
			}
			p += idSize
		}
	}
	return 0, ErrNoThumbnail
}

// itemProps carries the property boxes a picture needs to be decodable, copied
// verbatim rather than reinterpreted: the decoder configuration in particular
// is opaque and must survive unchanged.
type itemProps struct {
	Boxes         [][]byte
	Essential     []bool
	Width, Height int
}

// itemProperties collects the properties associated with an item.
func itemProperties(buf []byte, start, end int, item uint32) (itemProps, error) {
	iprp, ok := child(buf, start, end, "iprp")
	if !ok {
		return itemProps{}, ErrNoThumbnail
	}
	ipco, ok := child(buf, iprp.Start, iprp.End, "ipco")
	if !ok {
		return itemProps{}, ErrNoThumbnail
	}
	ipma, ok := child(buf, iprp.Start, iprp.End, "ipma")
	if !ok || ipma.End-ipma.Start < 8 {
		return itemProps{}, ErrNoThumbnail
	}
	available := parseBoxes(buf, ipco.Start, ipco.End)

	version := buf[ipma.Start]
	flags := readUint(buf, ipma.Start+1, 3)
	p := ipma.Start + 4
	count := int(binary.BigEndian.Uint32(buf[p : p+4]))
	p += 4
	if count > maxItems {
		return itemProps{}, ErrNoThumbnail
	}
	idSize := 2
	if version >= 1 {
		idSize = 4
	}

	var out itemProps
	for range count {
		if p+idSize+1 > ipma.End {
			break
		}
		id := readUint(buf, p, idSize)
		p += idSize
		n := int(buf[p])
		p++

		for range n {
			var essential bool
			var index int
			if flags&1 != 0 {
				if p+2 > ipma.End {
					return itemProps{}, ErrNoThumbnail
				}
				v := binary.BigEndian.Uint16(buf[p : p+2])
				essential, index = v&0x8000 != 0, int(v&0x7FFF)
				p += 2
			} else {
				if p+1 > ipma.End {
					return itemProps{}, ErrNoThumbnail
				}
				v := buf[p]
				essential, index = v&0x80 != 0, int(v&0x7F)
				p++
			}
			if id != item || index < 1 || index > len(available) {
				continue
			}
			b := available[index-1]
			out.Boxes = append(out.Boxes, buf[b.BoxStart:b.End2])
			out.Essential = append(out.Essential, essential)
			if b.Type == "ispe" && b.End-b.Start >= 12 {
				out.Width = int(binary.BigEndian.Uint32(buf[b.Start+4 : b.Start+8]))
				out.Height = int(binary.BigEndian.Uint32(buf[b.Start+8 : b.Start+12]))
			}
		}
	}
	if len(out.Boxes) == 0 || out.Width <= 0 || out.Height <= 0 {
		return itemProps{}, ErrNoThumbnail
	}
	return out, nil
}

// extent is one run of bytes belonging to an item.
type extent struct{ Offset, Length int64 }

// itemExtents finds where an item's data lives in the file.
func itemExtents(buf []byte, start, end int, item uint32) ([]extent, error) {
	iloc, ok := child(buf, start, end, "iloc")
	if !ok || iloc.End-iloc.Start < 8 {
		return nil, ErrNoThumbnail
	}
	version := buf[iloc.Start]
	p := iloc.Start + 4

	offsetSize := int(buf[p] >> 4)
	lengthSize := int(buf[p] & 0xF)
	baseOffsetSize := int(buf[p+1] >> 4)
	indexSize := 0
	if version == 1 || version == 2 {
		indexSize = int(buf[p+1] & 0xF)
	}
	p += 2

	var count int
	if version < 2 {
		count = int(binary.BigEndian.Uint16(buf[p : p+2]))
		p += 2
	} else {
		count = int(binary.BigEndian.Uint32(buf[p : p+4]))
		p += 4
	}
	if count > maxItems {
		return nil, ErrNoThumbnail
	}

	for range count {
		idSize := 2
		if version >= 2 {
			idSize = 4
		}
		if p+idSize > iloc.End {
			break
		}
		id := readUint(buf, p, idSize)
		p += idSize

		var method uint32
		if version == 1 || version == 2 {
			if p+2 > iloc.End {
				break
			}
			method = readUint(buf, p, 2) & 0xF
			p += 2
		}
		p += 2 // data reference index
		base := int64(readUint64(buf, p, baseOffsetSize))
		p += baseOffsetSize

		if p+2 > iloc.End {
			break
		}
		n := int(binary.BigEndian.Uint16(buf[p : p+2]))
		p += 2

		var extents []extent
		var total int64
		for range n {
			p += indexSize
			if p+offsetSize+lengthSize > iloc.End {
				return nil, ErrNoThumbnail
			}
			off := int64(readUint64(buf, p, offsetSize))
			p += offsetSize
			length := int64(readUint64(buf, p, lengthSize))
			p += lengthSize
			extents = append(extents, extent{Offset: base + off, Length: length})
			total += length
		}
		if id != item {
			continue
		}
		// Only data stored in the file itself is handled. The alternative puts
		// it in a metadata box or another file, which no camera does for a
		// thumbnail and which is not worth the code to find out.
		if method != 0 {
			return nil, ErrNoThumbnail
		}
		if total <= 0 || total > maxThumbnailSize {
			return nil, ErrNoThumbnail
		}
		return extents, nil
	}
	return nil, ErrNoThumbnail
}

// readExtents pulls an item's bytes out of the file.
func readExtents(r io.ReadSeeker, extents []extent) ([]byte, error) {
	var total int64
	for _, e := range extents {
		total += e.Length
	}
	out := make([]byte, 0, total)
	for _, e := range extents {
		if _, err := r.Seek(e.Offset, io.SeekStart); err != nil {
			return nil, err
		}
		chunk := make([]byte, e.Length)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, ErrNoThumbnail
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// buildHEIF wraps a picture and its properties in a minimal container.
//
// The result is an ordinary HEIC file holding one picture, which is what makes
// this worth doing: it can be handed straight to a device that reads HEIC,
// with nothing decoded on the way.
func buildHEIF(props itemProps, payload []byte) ([]byte, error) {
	const itemID = 1

	var ipcoBody []byte
	for _, b := range props.Boxes {
		ipcoBody = append(ipcoBody, b...)
	}
	ipco := makeBox("ipco", ipcoBody)

	assoc := []byte{0, itemID, byte(len(props.Boxes))}
	for i := range props.Boxes {
		v := byte(i + 1)
		if props.Essential[i] {
			v |= 0x80
		}
		assoc = append(assoc, v)
	}
	ipma := makeFullBox("ipma", 0, 0, append(be32(1), assoc...))
	iprp := makeBox("iprp", append(ipco, ipma...))

	hdlr := makeFullBox("hdlr", 0, 0, append(append(make([]byte, 4), []byte("pict")...),
		append(make([]byte, 12), 0)...))
	pitm := makeFullBox("pitm", 0, 0, be16(itemID))
	infe := makeFullBox("infe", 2, 0,
		append(append(be16(itemID), be16(0)...), append([]byte("hvc1"), 0)...))
	iinf := makeFullBox("iinf", 0, 0, append(be16(1), infe...))

	// The item's offset depends on how large everything before it is, and the
	// box recording that offset is itself part of that. Built twice: once to
	// learn the size, once for real.
	build := func(dataOffset uint32) []byte {
		body := []byte{0x44, 0x00} // four-byte offsets and lengths, no base
		body = append(body, be16(1)...)
		body = append(body, be16(itemID)...)
		body = append(body, be16(0)...) // data reference index
		body = append(body, be16(1)...) // one extent
		body = append(body, be32(dataOffset)...)
		body = append(body, be32(uint32(len(payload)))...)
		iloc := makeFullBox("iloc", 0, 0, body)

		meta := makeFullBox("meta", 0, 0,
			concat(hdlr, pitm, iinf, iprp, iloc))
		ftyp := makeBox("ftyp", concat([]byte("heic"), be32(0), []byte("mif1"), []byte("heic")))
		return append(ftyp, meta...)
	}

	head := build(0)
	// The payload sits immediately after the mdat header that follows the head.
	out := build(uint32(len(head) + 8))
	if len(out) != len(head) {
		return nil, errors.New("the container changed size between passes")
	}
	return append(out, makeBox("mdat", payload)...), nil
}

func makeBox(typ string, body []byte) []byte {
	out := make([]byte, 0, 8+len(body))
	out = append(out, be32(uint32(8+len(body)))...)
	out = append(out, typ...)
	return append(out, body...)
}

func makeFullBox(typ string, version byte, flags uint32, body []byte) []byte {
	head := append([]byte{version}, be32(flags)[1:]...)
	return makeBox(typ, append(head, body...))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func be16(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }

func be32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// readUint reads a big-endian integer of n bytes.
func readUint(buf []byte, at, n int) uint32 {
	return uint32(readUint64(buf, at, n))
}

func readUint64(buf []byte, at, n int) uint64 {
	var v uint64
	for i := range n {
		if at+i >= len(buf) {
			return v
		}
		v = v<<8 | uint64(buf[at+i])
	}
	return v
}
