package client

import (
	"bytes"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
)

// MultipartBuilder constructs a multipart/form-data body incrementally.
// Use NewMultipartBuilder, add fields/files, then call Finish to get the
// body bytes and Content-Type header value.
type MultipartBuilder struct {
	buf    bytes.Buffer
	writer *multipart.Writer
}

// NewMultipartBuilder creates a new multipart body builder.
func NewMultipartBuilder() *MultipartBuilder {
	mb := &MultipartBuilder{}
	mb.writer = multipart.NewWriter(&mb.buf)
	return mb
}

// AddField adds a text form field.
func (mb *MultipartBuilder) AddField(name, value string) error {
	return mb.writer.WriteField(name, value)
}

// AddFileFromPath adds a file field by reading from the filesystem.
func (mb *MultipartBuilder) AddFileFromPath(fieldName, filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	part, err := mb.writer.CreateFormFile(fieldName, filepath.Base(filePath))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, f)
	return err
}

// AddFileFromBytes adds a file field from an in-memory byte slice.
func (mb *MultipartBuilder) AddFileFromBytes(fieldName, fileName string, data []byte) error {
	part, err := mb.writer.CreateFormFile(fieldName, fileName)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

// AddFileFromReader adds a file field by reading from an io.Reader.
func (mb *MultipartBuilder) AddFileFromReader(fieldName, fileName string, reader io.Reader) error {
	part, err := mb.writer.CreateFormFile(fieldName, fileName)
	if err != nil {
		return err
	}
	_, err = io.Copy(part, reader)
	return err
}

// ContentType returns the Content-Type header value including the boundary.
// Must be called after Finish (or at any point for the current boundary).
func (mb *MultipartBuilder) ContentType() string {
	return mb.writer.FormDataContentType()
}

// Finish closes the multipart writer (writes the trailing boundary) and
// returns the complete body bytes and Content-Type header value.
func (mb *MultipartBuilder) Finish() (body []byte, contentType string, err error) {
	if err := mb.writer.Close(); err != nil {
		return nil, "", err
	}
	return mb.buf.Bytes(), mb.writer.FormDataContentType(), nil
}

// BuildMultipartRequest is a convenience that creates a Request with a
// multipart body from field and file data.
func BuildMultipartRequest(client *Client, method, urlOrPath string, fields map[string]string, files map[string][]byte) (*Request, error) {
	mb := NewMultipartBuilder()
	for k, v := range fields {
		if err := mb.AddField(k, v); err != nil {
			return nil, err
		}
	}
	for name, data := range files {
		if err := mb.AddFileFromBytes(name, name, data); err != nil {
			return nil, err
		}
	}
	body, contentType, err := mb.Finish()
	if err != nil {
		return nil, err
	}
	req := client.AcquireRequest()
	req.Method = method
	if len(urlOrPath) > 7 && (urlOrPath[:7] == "http://" || (len(urlOrPath) > 8 && urlOrPath[:8] == "https://")) {
		req.URL = urlOrPath
	} else {
		req.Path = urlOrPath
	}
	req.Body = body
	req.SetHeader("Content-Type", contentType)
	return req, nil
}
