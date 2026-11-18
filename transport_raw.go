// transport_raw.go — variant A's transport: a hand-rolled HTTP/1.1 server on raw TCP.
// No net/http in the serving path: request lines and headers are parsed by hand,
// bodies are read by content-length or chunked framing, and responses are written
// with pre-sized buffers. One goroutine per connection, keep-alive throughout.

//go:build transport_raw

package main

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func serve(ln net.Listener, handler func(*httpRequest) httpResponse) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetNoDelay(true)
		}
		go serveConn(conn, handler)
	}
}

func serveConn(conn net.Conn, handler func(*httpRequest) httpResponse) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 64*1024)
	writer := bufio.NewWriterSize(conn, 64*1024)
	for {
		req, keepAlive, err := readRequest(reader, conn)
		if err != nil || req == nil {
			return
		}
		resp := handler(req)
		if err := writeResponse(writer, resp, keepAlive); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// Read one request; returns (nil, false, nil) on a clean EOF before any bytes.
func readRequest(reader *bufio.Reader, conn net.Conn) (*httpRequest, bool, error) {
	line, err := readLine(reader)
	if err != nil {
		return nil, false, err
	}
	if line == "" {
		// tolerate a stray CRLF between requests
		line, err = readLine(reader)
		if err != nil {
			return nil, false, err
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, io.ErrUnexpectedEOF
	}
	method, target, version := parts[0], parts[1], parts[2]
	headers := map[string]string{}
	contentLength := -1
	chunked := false
	expectContinue := false
	connection := ""
	for {
		headerLine, err := readLine(reader)
		if err != nil {
			return nil, false, err
		}
		if headerLine == "" {
			break
		}
		colon := strings.IndexByte(headerLine, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(headerLine[:colon]))
		value := strings.TrimSpace(headerLine[colon+1:])
		switch name {
		case "content-length":
			if n, err := strconv.Atoi(value); err == nil {
				contentLength = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "expect":
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
			}
		case "connection":
			connection = strings.ToLower(value)
		}
		if _, exists := headers[name]; !exists {
			headers[name] = value
		}
	}
	if expectContinue {
		if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, false, err
		}
	}
	var body []byte
	switch {
	case chunked:
		body, err = readChunked(reader)
		if err != nil {
			return nil, false, err
		}
	case contentLength > 0:
		if contentLength > maxBody {
			return nil, false, io.ErrUnexpectedEOF
		}
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, false, err
		}
	}
	keepAlive := true
	if version == "HTTP/1.0" {
		keepAlive = connection == "keep-alive"
	} else if connection == "close" {
		keepAlive = false
	}
	parsed, err := url.ParseRequestURI(target)
	if err != nil {
		// an unparseable target still routes (to a 404) rather than killing the conn
		parsed = &url.URL{Path: target}
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	return &httpRequest{
		method:  method,
		path:    path,
		query:   parsed.Query(),
		headers: headers,
		body:    body,
		host:    headers["host"],
	}, keepAlive, nil
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readChunked(reader *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		sizeLine, err := readLine(reader)
		if err != nil {
			return nil, err
		}
		if idx := strings.IndexByte(sizeLine, ';'); idx >= 0 {
			sizeLine = sizeLine[:idx]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil || size < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		if size == 0 {
			// trailers until the blank line
			for {
				trailer, err := readLine(reader)
				if err != nil {
					return nil, err
				}
				if trailer == "" {
					return body, nil
				}
			}
		}
		if int64(len(body))+size > maxBody {
			return nil, io.ErrUnexpectedEOF
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		// trailing CRLF after each chunk
		if _, err := readLine(reader); err != nil {
			return nil, err
		}
	}
}

var statusReasons = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func writeResponse(writer *bufio.Writer, resp httpResponse, keepAlive bool) error {
	reason := statusReasons[resp.status]
	if reason == "" {
		reason = "Status"
	}
	var head strings.Builder
	head.WriteString("HTTP/1.1 ")
	head.WriteString(strconv.Itoa(resp.status))
	head.WriteByte(' ')
	head.WriteString(reason)
	head.WriteString("\r\n")
	if ct := resp.headers["content-type"]; ct != "" {
		head.WriteString("content-type: ")
		head.WriteString(ct)
		head.WriteString("\r\n")
	}
	for name, value := range resp.headers {
		if name == "content-type" {
			continue
		}
		head.WriteString(name)
		head.WriteString(": ")
		head.WriteString(value)
		head.WriteString("\r\n")
	}
	head.WriteString("content-length: ")
	head.WriteString(strconv.Itoa(len(resp.body)))
	head.WriteString("\r\n")
	if keepAlive {
		head.WriteString("connection: keep-alive\r\n")
	} else {
		head.WriteString("connection: close\r\n")
	}
	head.WriteString("\r\n")
	if _, err := writer.WriteString(head.String()); err != nil {
		return err
	}
	_, err := writer.Write(resp.body)
	return err
}
