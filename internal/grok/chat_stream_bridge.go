package grok

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type streamingChatWriter struct {
	header          http.Header
	committedHeader http.Header
	pipe            *io.PipeWriter
	status          int
	once            sync.Once
	ready           chan struct{}
}

func newStreamingChatWriter(pipe *io.PipeWriter) *streamingChatWriter {
	return &streamingChatWriter{header: make(http.Header), pipe: pipe, ready: make(chan struct{})}
}

func (w *streamingChatWriter) Header() http.Header { return w.header }
func (w *streamingChatWriter) WriteHeader(status int) {
	w.once.Do(func() { w.status = status; w.committedHeader = w.header.Clone(); close(w.ready) })
}
func (w *streamingChatWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pipe.Write(data)
}
func (w *streamingChatWriter) Flush() { w.WriteHeader(http.StatusOK) }

// streamThroughChat runs chat with a streaming ResponseWriter and hands the
// status, the committed headers and the body reader to consume. The producer is
// cancelled when consume returns, so a client disconnect stops the upstream.
func streamThroughChat(req *http.Request, chat http.HandlerFunc, consume func(int, http.Header, io.Reader)) {
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	streamWriter := newStreamingChatWriter(writer)
	go func() {
		defer writer.Close()
		chat(streamWriter, req.Clone(ctx))
		streamWriter.WriteHeader(http.StatusOK)
	}()
	select {
	case <-streamWriter.ready:
		consume(streamWriter.status, streamWriter.committedHeader, reader)
	case <-ctx.Done():
	}
}

// withChatStream owns the internal request and pipe lifecycle for both public
// protocol bridges. Returning from consume also cancels the upstream producer.
func (h *Handler) withChatStream(req *http.Request, consume func(int, http.Header, io.Reader)) {
	streamThroughChat(req, h.HandleChatCompletions, consume)
}
