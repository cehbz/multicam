package sony

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Liveview payload types.
const (
	PayloadJPEG      byte = 0x01
	PayloadFrameInfo byte = 0x02
)

// ErrLiveviewFraming reports a stream that doesn't follow the liveview packet
// layout; the reader can't resynchronise after it.
var ErrLiveviewFraming = errors.New("liveview framing")

const (
	commonHeaderLen  = 8
	payloadHeaderLen = 128
)

var payloadStartCode = [4]byte{0x24, 0x35, 0x68, 0x79}

// Frame is one liveview packet. Data is the JPEG image for PayloadJPEG and the
// frame-information records for PayloadFrameInfo; PayloadHeader is the full
// 128-byte payload header.
type Frame struct {
	Type          byte
	Sequence      uint16
	Timestamp     uint32 // milliseconds, camera clock
	PayloadHeader []byte
	Data          []byte
}

// LiveviewReader splits a liveview HTTP body into packets: an 8-byte common
// header (0xFF, payload type, sequence, timestamp), a 128-byte payload header
// (start code 24 35 68 79, 3-byte data size, 1-byte padding size), the data,
// then the padding.
type LiveviewReader struct {
	r *bufio.Reader
}

func NewLiveviewReader(r io.Reader) *LiveviewReader {
	return &LiveviewReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next packet. It returns io.EOF at a clean end of stream
// and io.ErrUnexpectedEOF when the stream ends inside a packet.
func (l *LiveviewReader) Next() (*Frame, error) {
	var ch [commonHeaderLen]byte
	if _, err := io.ReadFull(l.r, ch[:]); err != nil {
		return nil, err
	}
	if ch[0] != 0xFF {
		return nil, fmt.Errorf("%w: start byte %#02x", ErrLiveviewFraming, ch[0])
	}
	f := &Frame{
		Type:      ch[1],
		Sequence:  binary.BigEndian.Uint16(ch[2:4]),
		Timestamp: binary.BigEndian.Uint32(ch[4:8]),
	}
	if f.Type != PayloadJPEG && f.Type != PayloadFrameInfo {
		return nil, fmt.Errorf("%w: payload type %#02x", ErrLiveviewFraming, f.Type)
	}
	f.PayloadHeader = make([]byte, payloadHeaderLen)
	if _, err := io.ReadFull(l.r, f.PayloadHeader); err != nil {
		return nil, noEOF(err)
	}
	if [4]byte(f.PayloadHeader[:4]) != payloadStartCode {
		return nil, fmt.Errorf("%w: payload start code % x", ErrLiveviewFraming, f.PayloadHeader[:4])
	}
	size := int(f.PayloadHeader[4])<<16 | int(f.PayloadHeader[5])<<8 | int(f.PayloadHeader[6])
	padding := int(f.PayloadHeader[7])
	f.Data = make([]byte, size)
	if _, err := io.ReadFull(l.r, f.Data); err != nil {
		return nil, noEOF(err)
	}
	if _, err := l.r.Discard(padding); err != nil {
		return nil, noEOF(err)
	}
	return f, nil
}

func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
