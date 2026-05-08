package client

import (
	"bytes"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
)

type MultipartBuilder struct {
	buf    bytes.Buffer
	writer *multipart.Writer
}

func NewMultipartBuilder() *MultipartBuilder {
	mb := &MultipartBuilder{}
	mb.writer = multipart.NewWriter(&mb.buf)
	return mb
}

func (mb *MultipartBuilder) AddField(name, value string) error {
	return mb.writer.WriteField(name, value)
}

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

func (mb *MultipartBuilder) AddFileFromBytes(fieldName, fileName string, data []byte) error {
	part, err := mb.writer.CreateFormFile(fieldName, fileName)
	if err != nil {
		return err
	}
	_, err = part.Write(data)
	return err
}

func (mb *MultipartBuilder) AddFileFromReader(fieldName, fileName string, reader io.Reader) error {
	part, err := mb.writer.CreateFormFile(fieldName, fileName)
	if err != nil {
		return err
	}
	_, err = io.Copy(part, reader)
	return err
}

func (mb *MultipartBuilder) ContentType() string {
	return mb.writer.FormDataContentType()
}

func (mb *MultipartBuilder) Finish() (body []byte, contentType string, err error) {
	if err := mb.writer.Close(); err != nil {
		return nil, "", err
	}
	return mb.buf.Bytes(), mb.writer.FormDataContentType(), nil
}

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
