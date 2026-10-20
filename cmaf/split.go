package cmaf

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// SplitInit splits a CMAF track file (a header followed by fragments, as a
// segment response is) into the header (ftyp+moov) and the rest, at the first
// styp, sidx or moof box.
func SplitInit(b []byte) (init, rest []byte, err error) {
	pos := 0
	for pos+8 <= len(b) {
		size := int64(binary.BigEndian.Uint32(b[pos:]))
		typ := string(b[pos+4 : pos+8])
		hdr := int64(8)
		if size == 1 {
			if pos+16 > len(b) {
				return nil, nil, errors.New("truncated large box")
			}
			size = int64(binary.BigEndian.Uint64(b[pos+8:]))
			hdr = 16
		}
		switch typ {
		case "styp", "sidx", "moof":
			if pos == 0 {
				return nil, nil, errors.New("no header before the first fragment")
			}
			return b[:pos], b[pos:], nil
		}
		if size < hdr || int64(pos)+size > int64(len(b)) {
			return nil, nil, fmt.Errorf("box %q at %d: bad size %d", typ, pos, size)
		}
		pos += int(size)
	}
	return nil, nil, errors.New("no fragment after the header")
}
