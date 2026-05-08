package client

import (
	"bytes"
	"compress/gzip"
	"sync"
)

type Compressor struct {
	level int
	pool  sync.Pool
}

func NewCompressor(level int) *Compressor {
	if level < 1 || level > 9 {
		level = 6
	}

	c := &Compressor{
		level: level,
	}

	c.pool = sync.Pool{
		New: func() interface{} {
			var buf bytes.Buffer
			writer, _ := gzip.NewWriterLevel(&buf, level)
			return &gzipWriter{
				writer: writer,
				buf:    &buf,
			}
		},
	}

	return c
}

func (c *Compressor) CompressInto(dst []byte, src []byte) ([]byte, error) {
	if len(src) == 0 {
		return src, nil
	}

	gw := c.pool.Get().(*gzipWriter)
	defer c.pool.Put(gw)

	gw.buf.Reset()
	gw.writer.Reset(gw.buf)

	if _, err := gw.writer.Write(src); err != nil {
		return nil, err
	}

	if err := gw.writer.Close(); err != nil {
		return nil, err
	}

	compressed := gw.buf.Bytes()
	if len(compressed) > len(dst) {
		return nil, ErrRequestTooLarge
	}

	copy(dst, compressed)
	return dst[:len(compressed)], nil
}

type gzipWriter struct {
	writer *gzip.Writer
	buf    *bytes.Buffer
}
