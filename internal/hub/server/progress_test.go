package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
)

func TestProgressWriterRenewsPerChunk(t *testing.T) {
	recorder := &deadlineRecorder{ResponseWriter: newTestRecorder()}
	writer := &progressWriter{
		ResponseWriter: recorder,
		controller:     http.NewResponseController(recorder),
		timeout:        time.Minute,
	}
	// A single 8 MiB handler write must be split into chunks, each renewing the
	// deadline instead of relying on one fixed deadline for the whole body.
	payload := bytes.Repeat([]byte("x"), 8<<20)
	written, err := writer.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if written != len(payload) {
		t.Fatalf("wrote %d bytes, want %d", written, len(payload))
	}
	wantRenewals := len(payload) / writeChunkBytes
	if recorder.deadlines < wantRenewals {
		t.Fatalf("deadlines renewed %d times, want at least %d", recorder.deadlines, wantRenewals)
	}
}

func TestSlowProgressingDownloadIsNotTruncated(t *testing.T) {
	const (
		payloadBytes = 8 << 20
		idleTimeout  = 1500 * time.Millisecond
		// Read at 2 MiB/s so each 64 KiB server chunk lands well inside the
		// shortened idle timeout while the whole transfer takes far longer than
		// it: a single fixed deadline would cut the download after 500 ms.
		readChunkBytes = 32 << 10
		readInterval   = 16 * time.Millisecond
	)
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 0}, func(options *Options) {
		options.ResponseWriteTimeout = idleTimeout
	})
	payload := bytes.Repeat([]byte("rigscope\n"), payloadBytes/9)
	if err := server.Mount("/download", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = io.Copy(w, bytes.NewReader(payload))
	})); err != nil {
		t.Fatal(err)
	}
	startServer(t, server)

	connection, reader := dialDownload(t, server)
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(60 * time.Second))

	started := time.Now()
	buffer := make([]byte, readChunkBytes)
	received := 0
	ticker := time.NewTicker(readInterval)
	defer ticker.Stop()
	for {
		n, err := reader.Read(buffer)
		received += n
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("read after %d bytes: %v", received, err)
		}
		<-ticker.C
	}
	elapsed := time.Since(started)
	if received < len(payload) {
		t.Fatalf("received %d bytes, want the whole %d-byte body", received, len(payload))
	}
	if elapsed <= idleTimeout {
		t.Fatalf("transfer took %v; the test must outlast the idle timeout %v", elapsed, idleTimeout)
	}
}

func TestStalledDownloadIsCutOff(t *testing.T) {
	const (
		payloadBytes = 64 << 20
		idleTimeout  = 200 * time.Millisecond
	)
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 0}, func(options *Options) {
		options.ResponseWriteTimeout = idleTimeout
	})
	if err := server.Mount("/download", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(payloadBytes))
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for remaining := payloadBytes; remaining > 0; remaining -= len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})); err != nil {
		t.Fatal(err)
	}
	startServer(t, server)

	connection, reader := dialDownload(t, server)
	defer connection.Close()

	// Never read. The socket buffers fill, the next chunk blocks, and the idle
	// deadline that was renewed for it fires.
	time.Sleep(3 * idleTimeout)
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	received, err := io.Copy(io.Discard, reader)
	if err == nil {
		t.Fatal("a non-reading client consumed the entire response")
	}
	if received >= payloadBytes {
		t.Fatalf("received the whole %d-byte body despite not reading", payloadBytes)
	}
}

// dialDownload starts one download request and returns the raw connection plus a
// buffered reader positioned at the response body.
func dialDownload(t *testing.T, server *Server) (net.Conn, io.Reader) {
	t.Helper()
	address := server.Addr().String()
	connection, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	host := address
	if _, err := fmt.Fprintf(connection, "GET /download HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host); err != nil {
		t.Fatalf("write request: %v", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		connection.Close()
		t.Fatalf("read response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		connection.Close()
		t.Fatalf("status %d", response.StatusCode)
	}
	return connection, response.Body
}
