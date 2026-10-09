package syslogreceiver

import (
	"bufio"
	"errors"
	"io"
)

var (
	ErrFraming   = errors.New("syslog_malformed_framing")
	ErrOversized = errors.New("syslog_oversized_frame")
)

// ReadFrame consumes exactly one frame. Octet lengths are checked before any
// body allocation. The caller bounds the lifetime of partial frames with I/O
// deadlines; TCP read/packet boundaries are intentionally irrelevant.
func ReadFrame(r *bufio.Reader, mode string, maxBytes int) ([]byte, error) {
	if maxBytes < 1 || maxBytes > 65536 {
		return nil, ErrFraming
	}
	if mode == "lf" {
		data := make([]byte, 0, min(4096, maxBytes))
		for {
			b, err := r.ReadByte()
			if err != nil {
				if len(data) > 0 && errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return nil, err
			}
			if b == '\n' {
				if len(data) == 0 {
					return nil, ErrFraming
				}
				return data, nil
			}
			if len(data) == maxBytes {
				return nil, ErrOversized
			}
			data = append(data, b)
		}
	}
	if mode != "octet_counting" {
		return nil, ErrFraming
	}
	n, digits := 0, 0
	for {
		b, err := r.ReadByte()
		if err != nil {
			if digits > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if b == ' ' {
			if digits == 0 || n == 0 {
				return nil, ErrFraming
			}
			break
		}
		if b < '0' || b > '9' || digits == 0 && b == '0' {
			return nil, ErrFraming
		}
		digit := int(b - '0')
		if digits >= 10 || n > maxBytes/10 || n == maxBytes/10 && digit > maxBytes%10 {
			return nil, ErrOversized
		}
		n = n*10 + digit
		digits++
	}
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	return data, err
}
