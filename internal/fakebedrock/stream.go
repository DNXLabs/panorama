// SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
// SPDX-License-Identifier: AGPL-3.0-only

package fakebedrock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream/eventstreamapi"
)

// maxBodyBytes bounds what this server will read from one request. A test that
// sends more than this has gone wrong somewhere else.
const maxBodyBytes = 32 << 20

// contentTypeEventStream is what a streaming invocation answers with.
const contentTypeEventStream = "application/vnd.amazon.eventstream"

func readBody(r *http.Request) (json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("body is not JSON")
	}
	return json.RawMessage(raw), nil
}

// payloadPart is the frame payload Bedrock wraps each event in.
//
// encoding/json renders a []byte as a base64 string, which is exactly the shape
// the SDK's deserializer decodes - so the nesting here is the real one and not
// an approximation of it.
type payloadPart struct {
	Bytes []byte `json:"bytes"`
}

// writeCompletion answers a non-streaming invocation.
func (s *Server) writeCompletion(w http.ResponseWriter) {
	usage := s.currentUsage()
	body := map[string]any{
		"id":            "msg_fake",
		"type":          "message",
		"role":          "assistant",
		"model":         "fake",
		"content":       []any{map[string]string{"type": "text", "text": s.currentText()}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         usageJSON(usage),
	}
	writeJSON(w, body)
}

// writeStream answers a streaming invocation with event-stream frames.
//
// Every frame is flushed as it is written. Without that the whole response
// arrives at once and the test that proves the proxy does not buffer would pass
// against a server that buffered, which is the one thing it exists to rule out.
func (s *Server) writeStream(w http.ResponseWriter, failMidway bool) {
	usage := s.currentUsage()
	delay := s.currentDelay()

	w.Header().Set("Content-Type", contentTypeEventStream)
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// A flush failure is kept rather than discarded. A ResponseWriter that
	// cannot flush is a legitimate caller - a recorder is one - and that is not
	// a failure. A connection that has gone away is, and swallowing it let this
	// server report a stream "written in full" that never left the building,
	// which is the one claim its accounting exists to make. The first is kept,
	// because every flush after a broken pipe reports the same thing.
	var flushErr error
	flush := func() {
		err := rc.Flush()
		if err == nil || errors.Is(err, http.ErrNotSupported) || flushErr != nil {
			return
		}
		flushErr = err
	}

	events := anthropicEvents(usage, s.currentText())
	if failMidway {
		// One well-formed opening event, then the exception. The response has
		// begun by then, so the data plane has nowhere to fall back to and has
		// to report the failure in band.
		events = events[:1]
	}

	written := 0
	for _, ev := range events {
		if err := writeChunk(w, ev); err != nil {
			s.streamEnded(written, len(events), err)
			return
		}
		written++
		flush()
		if delay > 0 {
			time.Sleep(delay)
		}
	}

	if failMidway {
		_ = writeException(w, "modelStreamErrorException", "fakebedrock: the model stream failed")
		flush()
	}
	s.streamEnded(written, len(events), flushErr)
}

// writeChunk frames one Anthropic event as a Bedrock chunk.
func writeChunk(w io.Writer, event []byte) error {
	payload, err := json.Marshal(payloadPart{Bytes: event})
	if err != nil {
		return fmt.Errorf("encoding payload part: %w", err)
	}
	return encode(w, eventstream.Message{
		Headers: eventstream.Headers{
			{Name: eventstreamapi.MessageTypeHeader, Value: eventstream.StringValue(eventstreamapi.EventMessageType)},
			{Name: eventstreamapi.EventTypeHeader, Value: eventstream.StringValue("chunk")},
			{Name: eventstreamapi.ContentTypeHeader, Value: eventstream.StringValue("application/json")},
		},
		Payload: payload,
	})
}

// writeException frames a mid-stream failure.
func writeException(w io.Writer, exceptionType, message string) error {
	payload, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return fmt.Errorf("encoding exception: %w", err)
	}
	return encode(w, eventstream.Message{
		Headers: eventstream.Headers{
			{Name: eventstreamapi.MessageTypeHeader, Value: eventstream.StringValue(eventstreamapi.ExceptionMessageType)},
			{Name: eventstreamapi.ExceptionTypeHeader, Value: eventstream.StringValue(exceptionType)},
			{Name: eventstreamapi.ContentTypeHeader, Value: eventstream.StringValue("application/json")},
		},
		Payload: payload,
	})
}

// encode writes one framed message.
//
// The message is encoded into a buffer first and then written in one call. The
// encoder writes the prelude, the headers and two CRCs separately, and a short
// write partway through would leave a frame the decoder cannot resynchronise
// from - which surfaces as an unreadable stream rather than as a write error.
func encode(w io.Writer, msg eventstream.Message) error {
	var buf bytes.Buffer
	if err := eventstream.NewEncoder().Encode(&buf, msg); err != nil {
		return fmt.Errorf("encoding event-stream message: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("writing event-stream message: %w", err)
	}
	return nil
}

// usageJSON renders the accounting the way Bedrock reports it.
//
// Both the flat cache_creation_input_tokens and the cache_creation split are
// present, because that is what a current response carries and because
// eventStream.observe reconstructs one from the other when it is missing - a
// path worth leaving reachable rather than papering over here.
func usageJSON(u Usage) map[string]any {
	return map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadTokens,
		"cache_creation_input_tokens": u.Total(),
		"cache_creation": map[string]any{
			"ephemeral_5m_input_tokens": u.CacheWrite5mTokens,
			"ephemeral_1h_input_tokens": u.CacheWrite1hTokens,
		},
	}
}

// anthropicEvents is the sequence a real streamed completion produces.
//
// The shape matters more than the content: message_start carries the input and
// cache classes under message.usage, and message_delta restates output under a
// top-level usage. That is what eventStream.observe reads, and a fake that put
// the numbers anywhere else would report zero tokens while looking correct.
func anthropicEvents(u Usage, text string) [][]byte {
	must := func(v any) []byte {
		out, err := json.Marshal(v)
		if err != nil {
			// Every value here is a literal composed in this function; a
			// failure would be a programming error, not a runtime condition.
			panic(fmt.Sprintf("fakebedrock: encoding event: %v", err))
		}
		return out
	}

	start := must(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":      "msg_fake",
			"type":    "message",
			"role":    "assistant",
			"model":   "fake",
			"content": []any{},
			"usage": map[string]any{
				"input_tokens":                u.InputTokens,
				"output_tokens":               0,
				"cache_read_input_tokens":     u.CacheReadTokens,
				"cache_creation_input_tokens": u.Total(),
				"cache_creation": map[string]any{
					"ephemeral_5m_input_tokens": u.CacheWrite5mTokens,
					"ephemeral_1h_input_tokens": u.CacheWrite1hTokens,
				},
			},
		},
	})

	return [][]byte{
		start,
		must(map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]string{"type": "text", "text": ""},
		}),
		must(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "text_delta", "text": text},
		}),
		must(map[string]any{"type": "content_block_stop", "index": 0}),
		must(map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": u.OutputTokens},
		}),
		must(map[string]any{"type": "message_stop"}),
	}
}
