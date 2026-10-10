// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package bedrock

import (
	"context"
	"log/slog"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"
)

// connTrace records what the HTTP transport did with the connection one
// streamed invocation used.
//
// It exists for a failure whose error text names nothing. A streamed response
// that stops part way through reports "read tcp ...: use of closed network
// connection" - net.ErrClosed, which a socket only ever reports about itself.
// So the close came from inside this process, and the error says no more than
// that: not whether the connection was freshly dialled or taken from the idle
// pool, and not, which is the question, whether the transport had already
// handed it back to that pool while the response was still being read. A
// connection returned to the pool under an active reader is this process
// closing its own socket; one that was never returned is the far end hanging
// up. The two are indistinguishable without this and lead opposite ways.
//
// Streamed invocations only. A unary call reads its body to the end inside the
// SDK, so there is no window between the transport finishing with a connection
// and this code finishing with it - which is the whole of what this watches.
type connTrace struct {
	// id identifies this invocation across the two log lines that mention it.
	//
	// TEMPORARY. The write that fails and the stream that dies name the same
	// connection, and the whole question is whether they are the same request:
	// our own request being written again while its first response streams, or
	// a second request handed a connection that was not free. Those are
	// different faults with different fixes and nothing else distinguishes
	// them.
	id        uint64
	startedAt time.Time

	mu        sync.Mutex
	gotConn   bool
	local     string
	reused    bool
	wasIdle   bool
	idleFor   time.Duration
	firstByte time.Time

	// putIdle records the transport returning the connection to the idle pool.
	// Read at the moment a stream fails, it answers what the error cannot: a
	// connection already back in the pool is one the transport believed it had
	// finished with, while the reader had not.
	putIdle    bool
	putIdleErr error
}

// withConnTrace attaches the hooks to a context and returns what they fill in.
//
// The cost is a few field writes on connection acquisition and on the first
// response byte - once per request, not once per token - which is nothing
// against the latency budget this package is held to. Nothing is emitted unless
// the stream fails. Tracing only a sample of requests would be worse than not
// tracing at all, because the failure is rare and the trace is wanted for
// exactly the requests that have it.
// traceSeq numbers invocations for the id above.
var traceSeq atomic.Uint64

func withConnTrace(ctx context.Context, log *slog.Logger) (context.Context, *connTrace) {
	t := &connTrace{id: traceSeq.Add(1), startedAt: time.Now()}

	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// TEMPORARY, with the rest of the close diagnostic. net/http's
		// writeLoop closes a connection when writing a request on it returns
		// an error, and the trace has established that the socket write does
		// not fail - so the error comes from Request.write itself and is the
		// last unnamed thing in this failure. This hook carries exactly that
		// error, and it belongs to the request being written rather than to
		// the stream that dies, which is why it is logged here and now instead
		// of being folded into a failure that reports somebody else's request.
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				return
			}
			t.mu.Lock()
			conn := t.local
			t.mu.Unlock()
			log.Warn("upstream request write returned an error",
				"invocation", t.id, "local", conn, "error", info.Err.Error())
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.gotConn = true
			t.reused = info.Reused
			t.wasIdle = info.WasIdle
			t.idleFor = info.IdleTime
			if info.Conn != nil {
				t.local = info.Conn.LocalAddr().String()
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.firstByte = time.Now()
		},
		// Fires on the transport's own goroutine, which is why everything here
		// is behind a mutex: it races the reader by design, and that race is
		// the thing being measured.
		PutIdleConn: func(err error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.putIdle = true
			t.putIdleErr = err
		},
	}), t
}

// attrs renders the trace for a log line, and is safe on a nil receiver so a
// stream built without one still reports.
func (t *connTrace) attrs() []slog.Attr {
	if t == nil {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	out := []slog.Attr{
		slog.Uint64("invocation", t.id),
		slog.Bool("conn_reused", t.reused),
		slog.Bool("conn_was_idle", t.wasIdle),
		slog.Bool("conn_returned_to_pool", t.putIdle),
	}
	if t.wasIdle {
		out = append(out, slog.Duration("conn_idle_for", t.idleFor))
	}
	// A non-nil error here is the transport declining to keep the connection,
	// and it names the reason it would not.
	if t.putIdleErr != nil {
		out = append(out, slog.String("conn_pool_refused", t.putIdleErr.Error()))
	}
	if !t.firstByte.IsZero() {
		out = append(out, slog.Duration("conn_first_byte_after", t.firstByte.Sub(t.startedAt)))
	}
	// No connection was ever handed over, so the failure happened before there
	// was one - a different fault entirely, and worth not confusing with this.
	if !t.gotConn {
		out = append(out, slog.Bool("conn_never_acquired", true))
	}
	return out
}
