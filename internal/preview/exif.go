package preview

import (
	"encoding/binary"
	"errors"
)

// Mirage reads three things out of EXIF and nothing else: which way up the
// picture is, and where the camera left a thumbnail. A general EXIF library
// would be far larger than the hundred lines that answers those, and this way
// a malformed tag cannot do anything worse than leave a photograph sideways.

// Orientation is the EXIF orientation tag, which says how a picture must be
// turned before it is shown. Phones almost never rotate the pixels; they record
// how the phone was held and leave the decoding to whatever displays it.
type Orientation uint16

const (
	OrientationNormal Orientation = 1
	orientationMax    Orientation = 8
)

// exifData is what was found in a file's EXIF block.
type exifData struct {
	Orientation Orientation
	// ThumbOffset and ThumbLength locate a JPEG thumbnail inside the file, or
	// are zero if there is none. Reading it costs a few kilobytes where
	// decoding the picture costs tens of megabytes.
	ThumbOffset uint32
	ThumbLength uint32
}

// EXIF tag numbers.
const (
	tagOrientation  = 0x0112
	tagThumbOffset  = 0x0201
	tagThumbLength  = 0x0202
	tagExifIFD      = 0x8769
	maxIFDEntries   = 512
	maxIFDsFollowed = 8
)

var errNoEXIF = errors.New("no EXIF block")

// parseEXIF reads the EXIF block that begins after the "Exif\0\0" header.
func parseEXIF(b []byte) (exifData, error) {
	var out exifData
	if len(b) < 8 {
		return out, errNoEXIF
	}

	// A TIFF header: the byte order, the number 42 as a check, then the offset
	// of the first directory.
	var order binary.ByteOrder
	switch {
	case b[0] == 'I' && b[1] == 'I':
		order = binary.LittleEndian
	case b[0] == 'M' && b[1] == 'M':
		order = binary.BigEndian
	default:
		return out, errNoEXIF
	}
	if order.Uint16(b[2:4]) != 42 {
		return out, errNoEXIF
	}

	offset := order.Uint32(b[4:8])
	// IFD0 holds the orientation; IFD1, which follows it, holds the thumbnail.
	// Bounded because the offsets come from the file and a loop is one forged
	// pointer away.
	for range maxIFDsFollowed {
		if offset == 0 || int(offset)+2 > len(b) {
			break
		}
		next, err := readIFD(b, order, int(offset), &out)
		if err != nil {
			break
		}
		offset = next
	}
	return out, nil
}

// readIFD reads one image file directory, returning the offset of the next.
func readIFD(b []byte, order binary.ByteOrder, at int, out *exifData) (uint32, error) {
	if at+2 > len(b) {
		return 0, errNoEXIF
	}
	count := int(order.Uint16(b[at : at+2]))
	if count > maxIFDEntries {
		count = maxIFDEntries
	}

	pos := at + 2
	for range count {
		if pos+12 > len(b) {
			return 0, errNoEXIF
		}
		tag := order.Uint16(b[pos : pos+2])
		value := order.Uint32(b[pos+8 : pos+12])

		switch tag {
		case tagOrientation:
			// A short, so the value sits in the first half of the field, which
			// is where the byte order matters.
			v := Orientation(order.Uint16(b[pos+8 : pos+10]))
			if v >= OrientationNormal && v <= orientationMax {
				out.Orientation = v
			}
		case tagThumbOffset:
			out.ThumbOffset = value
		case tagThumbLength:
			out.ThumbLength = value
		case tagExifIFD:
			// The sub-directory is not followed: nothing Mirage wants lives in
			// it, and following offsets is where this gets expensive.
		}
		pos += 12
	}

	if pos+4 > len(b) {
		return 0, nil
	}
	return order.Uint32(b[pos : pos+4]), nil
}

// thumbnail returns the embedded JPEG, if the EXIF block located one.
func (e exifData) thumbnail(exifBlock []byte) []byte {
	if e.ThumbLength == 0 || e.ThumbOffset == 0 {
		return nil
	}
	start, end := int(e.ThumbOffset), int(e.ThumbOffset)+int(e.ThumbLength)
	if start >= len(exifBlock) || end > len(exifBlock) || end <= start {
		return nil
	}
	thumb := exifBlock[start:end]
	// Checked rather than trusted: the offsets are from the file, and handing
	// a decoder something that is not a JPEG is how it finds out the hard way.
	if len(thumb) < 2 || thumb[0] != 0xFF || thumb[1] != 0xD8 {
		return nil
	}
	return thumb
}
