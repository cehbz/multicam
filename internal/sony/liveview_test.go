package sony

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// packet builds one liveview packet per the layout in Sony's
// SimpleLiveviewSlicer.java and pysony: 8-byte common header (0xFF, payload
// type, big-endian sequence and millisecond timestamp), 128-byte payload header
// (start code 24 35 68 79, 3-byte data size, 1-byte padding size, 120 bytes
// type-specific/reserved), data, padding.
func packet(typ byte, seq uint16, ts uint32, data []byte, padding int) []byte {
	var b bytes.Buffer
	b.WriteByte(0xFF)
	b.WriteByte(typ)
	binary.Write(&b, binary.BigEndian, seq)
	binary.Write(&b, binary.BigEndian, ts)
	hdr := make([]byte, 128)
	copy(hdr, []byte{0x24, 0x35, 0x68, 0x79})
	hdr[4], hdr[5], hdr[6] = byte(len(data)>>16), byte(len(data)>>8), byte(len(data))
	hdr[7] = byte(padding)
	b.Write(hdr)
	b.Write(data)
	b.Write(make([]byte, padding))
	return b.Bytes()
}

func TestLiveviewReader(t *testing.T) {
	jpeg1 := append([]byte{0xFF, 0xD8}, bytes.Repeat([]byte{0xAB}, 70000)...) // > 64 KiB exercises the 3rd size byte
	jpeg2 := []byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9}
	info := bytes.Repeat([]byte{0x11}, 16)
	var stream bytes.Buffer
	stream.Write(packet(PayloadJPEG, 1, 1000, jpeg1, 4))
	stream.Write(packet(PayloadFrameInfo, 2, 1010, info, 0))
	stream.Write(packet(PayloadJPEG, 3, 1033, jpeg2, 255))

	r := NewLiveviewReader(&stream)
	want := []Frame{
		{Type: PayloadJPEG, Sequence: 1, Timestamp: 1000, Data: jpeg1},
		{Type: PayloadFrameInfo, Sequence: 2, Timestamp: 1010, Data: info},
		{Type: PayloadJPEG, Sequence: 3, Timestamp: 1033, Data: jpeg2},
	}
	for i, w := range want {
		f, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Type != w.Type || f.Sequence != w.Sequence || f.Timestamp != w.Timestamp || !bytes.Equal(f.Data, w.Data) {
			t.Errorf("frame %d = type %#x seq %d ts %d len %d, want type %#x seq %d ts %d len %d",
				i, f.Type, f.Sequence, f.Timestamp, len(f.Data), w.Type, w.Sequence, w.Timestamp, len(w.Data))
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("at end of stream: err = %v, want io.EOF", err)
	}
}

func TestLiveviewReaderErrors(t *testing.T) {
	good := packet(PayloadJPEG, 1, 0, []byte{1, 2, 3}, 0)
	badStart := append([]byte{}, good...)
	badStart[0] = 0xFE
	badCode := append([]byte{}, good...)
	badCode[8+3] = 0x00
	unknown := packet(0x12, 1, 0, []byte{1}, 0)
	tests := []struct {
		name   string
		stream []byte
		want   error
	}{
		{"bad start byte", badStart, ErrLiveviewFraming},
		{"bad start code", badCode, ErrLiveviewFraming},
		{"unknown payload type", unknown, ErrLiveviewFraming},
		{"truncated common header", good[:5], io.ErrUnexpectedEOF},
		{"truncated payload header", good[:50], io.ErrUnexpectedEOF},
		{"truncated data", good[:len(good)-1], io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLiveviewReader(bytes.NewReader(tt.stream)).Next()
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
