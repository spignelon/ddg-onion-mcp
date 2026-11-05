// SPDX-License-Identifier: GPL-3.0-or-later

package torhttp

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// When the profile sets Accept-Encoding itself, Go stops decompressing
// responses, so Do decodes gzip, deflate, br and zstd here. Callers bound the
// decoded size with io.LimitReader, which also bounds decompression bombs.

// zstdMaxWindow caps the memory a zstd stream may demand of the decoder.
const zstdMaxWindow = 8 << 20

type decodedBody struct {
	io.Reader
	closers []func() error
}

func (d *decodedBody) Close() error {
	var first error
	for i := len(d.closers) - 1; i >= 0; i-- {
		if err := d.closers[i](); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// decodeResponse replaces resp.Body with a decoding reader when the response
// carries a Content-Encoding the profile asked for.
func decodeResponse(resp *http.Response) error {
	raw := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if raw == "" || strings.EqualFold(raw, "identity") {
		return nil
	}
	parts := strings.Split(strings.ToLower(raw), ",")
	body := &decodedBody{Reader: resp.Body, closers: []func() error{resp.Body.Close}}
	var r io.Reader = resp.Body

	// Encodings were applied in listed order, so undo them in reverse.
	for i := len(parts) - 1; i >= 0; i-- {
		switch enc := strings.TrimSpace(parts[i]); enc {
		case "", "identity":
		case "gzip", "x-gzip":
			zr, err := gzip.NewReader(r)
			if err != nil {
				return fmt.Errorf("decoding gzip response: %w", err)
			}
			r = zr
			body.closers = append(body.closers, zr.Close)
		case "deflate":
			br := bufio.NewReader(r)
			head, _ := br.Peek(2)
			if len(head) == 2 && head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
				zr, err := zlib.NewReader(br) // RFC 1950, what servers should send
				if err != nil {
					return fmt.Errorf("decoding deflate response: %w", err)
				}
				r = zr
				body.closers = append(body.closers, zr.Close)
			} else {
				fr := flate.NewReader(br) // raw DEFLATE, which some servers send
				r = fr
				body.closers = append(body.closers, fr.Close)
			}
		case "br":
			r = brotli.NewReader(r)
		case "zstd":
			zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(zstdMaxWindow), zstd.WithDecoderLowmem(true))
			if err != nil {
				return fmt.Errorf("decoding zstd response: %w", err)
			}
			r = zr
			body.closers = append(body.closers, func() error { zr.Close(); return nil })
		default:
			return fmt.Errorf("unsupported Content-Encoding %q", enc)
		}
	}
	body.Reader = r
	resp.Body = body
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return nil
}
