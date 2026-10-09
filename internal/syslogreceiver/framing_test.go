package syslogreceiver

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// tinyReader models TCP splitting both the decimal header and body across reads.
type tinyReader struct {
	data []byte
	size int
}

func (r *tinyReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(r.data), r.size)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func octetFrame(message string) string { return fmt.Sprintf("%d %s", len(message), message) }

func TestReadFrameOctetCountingPacketIndependent(t *testing.T) {
	messages := []string{"<0>1 - - - - - -", "embedded\nnewline\x00", "привет"}
	var wire strings.Builder
	for _, msg := range messages {
		wire.WriteString(octetFrame(msg))
	}
	for _, readSize := range []int{1, 2, 7, 4096} {
		t.Run(fmt.Sprint(readSize), func(t *testing.T) {
			reader := bufio.NewReader(&tinyReader{data: []byte(wire.String()), size: readSize})
			for _, expected := range messages {
				frame, err := ReadFrame(reader, "octet_counting", 2048)
				if err != nil || string(frame) != expected {
					t.Fatalf("split header/body changed frame: %q %v", frame, err)
				}
			}
			if _, err := ReadFrame(reader, "octet_counting", 2048); !errors.Is(err, io.EOF) {
				t.Fatal("clean EOF not reported", err)
			}
		})
	}
}

func TestReadFrameManyMessagesOneConnection(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("1 x", 1000)))
	for range 1000 {
		frame, err := ReadFrame(reader, "octet_counting", 2048)
		if err != nil || string(frame) != "x" {
			t.Fatal("message boundary lost", string(frame), err)
		}
	}
}

func TestReadFrameMalformedAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, wire, mode string
		max              int
		want             error
	}{
		{"empty", "", "octet_counting", 2048, io.EOF},
		{"split incomplete header", "12", "octet_counting", 2048, io.ErrUnexpectedEOF},
		{"disconnect body", "5 ab", "octet_counting", 2048, io.ErrUnexpectedEOF},
		{"missing body", "5 ", "octet_counting", 2048, io.EOF},
		{"negative", "-1 x", "octet_counting", 2048, ErrFraming},
		{"plus", "+1 x", "octet_counting", 2048, ErrFraming},
		{"zero", "0 ", "octet_counting", 2048, ErrFraming},
		{"leading zero", "01 x", "octet_counting", 2048, ErrFraming},
		{"missing length", " x", "octet_counting", 2048, ErrFraming},
		{"malformed prefix", "1x x", "octet_counting", 2048, ErrFraming},
		{"newline prefix", "1\nx", "octet_counting", 2048, ErrFraming},
		{"huge overflow", strings.Repeat("9", 10000) + " x", "octet_counting", 2048, ErrOversized},
		{"above max", "2049 ", "octet_counting", 2048, ErrOversized},
		{"invalid mode", "1 x", "auto", 2048, ErrFraming},
		{"invalid max", "1 x", "octet_counting", 0, ErrFraming},
		{"above hard max", "1 x", "octet_counting", 65537, ErrFraming},
		{"LF partial", "partial", "lf", 2048, io.ErrUnexpectedEOF},
		{"LF empty", "\n", "lf", 2048, ErrFraming},
		{"LF oversized", strings.Repeat("x", 2049) + "\n", "lf", 2048, ErrOversized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := ReadFrame(bufio.NewReader(strings.NewReader(tc.wire)), tc.mode, tc.max)
			if !errors.Is(err, tc.want) || len(frame) > max(tc.max, 0) {
				t.Fatalf("unexpected error/allocation: len=%d err=%v want=%v", len(frame), err, tc.want)
			}
		})
	}
}

func TestReadFrameLFAndLengthMismatch(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("one\ntwo\r\n"))
	for _, expected := range []string{"one", "two\r"} {
		frame, err := ReadFrame(reader, "lf", 2048)
		if err != nil || string(frame) != expected {
			t.Fatal("LF delimiter changed payload octets", frame, err)
		}
	}
	// TCP cannot detect a lying length by packet boundaries. The declared octets
	// are consumed exactly; garbage at the next boundary closes the connection.
	reader = bufio.NewReader(strings.NewReader("2 abc1 x"))
	frame, err := ReadFrame(reader, "octet_counting", 2048)
	if err != nil || string(frame) != "ab" {
		t.Fatal("declared boundary ignored", frame, err)
	}
	if _, err := ReadFrame(reader, "octet_counting", 2048); !errors.Is(err, ErrFraming) {
		t.Fatal("length mismatch tail accepted", err)
	}
	for _, mode := range []string{"lf", "octet_counting"} {
		message := strings.Repeat("x", 2048)
		wire := octetFrame(message)
		if mode == "lf" {
			wire = message + "\n"
		}
		frame, err := ReadFrame(bufio.NewReader(strings.NewReader(wire)), mode, 2048)
		if err != nil || string(frame) != message {
			t.Fatal("maximum-size frame rejected", mode, err)
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	for _, data := range [][]byte{[]byte("1 x"), []byte("17 embedded\nnewline"), []byte("0 "), []byte("999999999999999999 "), []byte("hello\n"), []byte("1 \xff")} {
		f.Add(data, false)
		f.Add(data, true)
	}
	f.Fuzz(func(t *testing.T, data []byte, lf bool) {
		if len(data) > 65536 {
			t.Skip()
		}
		mode := "octet_counting"
		if lf {
			mode = "lf"
		}
		frame, err := ReadFrame(bufio.NewReader(&tinyReader{data: data, size: 3}), mode, 2048)
		if len(frame) > 2048 {
			t.Fatal("frame exceeded allocation bound")
		}
		if err == nil {
			if len(frame) == 0 || !bytes.Contains(data, frame) {
				t.Fatal("successful frame changed source octets")
			}
		}
	})
}
