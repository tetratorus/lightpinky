package ios

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// mjpegReader reads WebDriverAgent's MJPEG stream: an HTTP response of type
// multipart/x-mixed-replace whose parts are each one JPEG, framed as
//
//	--BoundaryString\r\n
//	Content-type: image/jpeg\r\n
//	Content-Length: 79549\r\n
//	\r\n
//	<JPEG bytes>
//
// It is lenient about the blank lines around parts and finds the end of a
// part without Content-Length by its JPEG end marker.
type mjpegReader struct {
	r    *bufio.Reader
	body io.Closer
	// upside, when set, is the turn frames whose EXIF says up need: XCTest
	// tags an upside-down screen as up (WDADevice.pictureTurn).
	upside func() quarter
}

// maxJPEG bounds one frame: a full-size screenshot at high quality is a few
// megabytes.
const maxJPEG = 32 << 20

func newMJPEGReader(r io.Reader, c io.Closer) *mjpegReader {
	return &mjpegReader{r: bufio.NewReaderSize(r, 64<<10), body: c}
}

// openMJPEG connects to an MJPEG server and returns its frames.
func openMJPEG(ctx context.Context, hc *http.Client, url string) (frameSource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MJPEG: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("MJPEG: HTTP %d", resp.StatusCode)
	}
	return newMJPEGReader(resp.Body, resp.Body), nil
}

// nextJPEG returns the next part's bytes.
func (m *mjpegReader) nextJPEG() ([]byte, error) {
	length := -1
	inHeaders := false
	for {
		line, err := m.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "--"):
			inHeaders, length = true, -1
		case inHeaders && line == "":
			return m.body2(length)
		case inHeaders:
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 && n <= maxJPEG {
					length = n
				}
			}
		}
	}
}

// body2 reads a part's body: length bytes, or up to the JPEG end marker
// when the length is unknown.
func (m *mjpegReader) body2(length int) ([]byte, error) {
	if length >= 0 {
		b := make([]byte, length)
		if _, err := io.ReadFull(m.r, b); err != nil {
			return nil, err
		}
		return b, nil
	}
	var buf bytes.Buffer
	var prev byte
	for buf.Len() <= maxJPEG {
		c, err := m.r.ReadByte()
		if err != nil {
			return nil, err
		}
		buf.WriteByte(c)
		if prev == 0xFF && c == 0xD9 {
			return buf.Bytes(), nil
		}
		prev = c
	}
	return nil, errors.New("MJPEG: frame too large")
}

func (m *mjpegReader) next(ctx context.Context) (frame, error) {
	if err := ctx.Err(); err != nil {
		return frame{}, err
	}
	b, err := m.nextJPEG()
	if err != nil {
		if ctx.Err() != nil {
			return frame{}, ctx.Err()
		}
		return frame{}, fmt.Errorf("MJPEG: %w", err)
	}
	turn := exifTurn(b)
	if turn == 0 && m.upside != nil {
		turn = m.upside()
	}
	return frame{key: frameKey(b), turn: turn, decode: func() (image.Image, error) { return jpeg.Decode(bytes.NewReader(b)) }}, nil
}

func (m *mjpegReader) Close() error {
	if m.body != nil {
		return m.body.Close()
	}
	return nil
}
