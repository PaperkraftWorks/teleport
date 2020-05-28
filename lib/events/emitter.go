/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package events

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

// CheckingEmitterConfig provides parameters for emitter
type CheckingEmitterConfig struct {
	// Inner emits events to the underlying store
	Inner Emitter
	// Clock is a clock interface, used in tests
	Clock clockwork.Clock
	// UIDGenerator is unique ID generator
	UIDGenerator utils.UID
}

// NewCheckingEmitter returns emitter that checks
// that all required fields are properly set
func NewCheckingEmitter(cfg CheckingEmitterConfig) (*CheckingEmitter, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	return &CheckingEmitter{
		CheckingEmitterConfig: cfg,
	}, nil
}

// CheckingEmitter ensures that event fields have been set properly
// and reports statistics for every wrapper
type CheckingEmitter struct {
	CheckingEmitterConfig
}

// CheckAndSetDefaults checks and sets default values
func (w *CheckingEmitterConfig) CheckAndSetDefaults() error {
	if w.Inner == nil {
		return trace.BadParameter("missing parameter Inner")
	}
	if w.Clock == nil {
		w.Clock = clockwork.NewRealClock()
	}
	if w.UIDGenerator == nil {
		w.UIDGenerator = utils.NewRealUID()
	}
	return nil
}

// EmitAuditEvent emits audit event
func (r *CheckingEmitter) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	if err := CheckAndSetEventFields(event, r.Clock, r.UIDGenerator); err != nil {
		log.WithError(err).Errorf("Failed to emit audit event.")
		auditFailedEmit.Inc()
		return trace.Wrap(err)
	}
	if err := r.Inner.EmitAuditEvent(ctx, event); err != nil {
		auditFailedEmit.Inc()
		log.WithError(err).Errorf("Failed to emit audit event.")
		if event.GetType() == SessionEndEvent {
			debug.PrintStack()
			log.Errorf("EMITTER ERR sesh end %v %v %v.", SessionEndEvent, event, err)
		}
		return trace.Wrap(err)
	}
	if event.GetType() == SessionEndEvent {
		debug.PrintStack()
		log.Errorf("EMITTER OK sesh end %v %v.", SessionEndEvent, event)
	}
	return nil
}

// CheckAndSetEventFields updates passed event fields with additional information
// common for all event types such as unique IDs, timestamps, codes, etc.
//
// This method is a "final stop" for various audit log implementations for
// updating event fields before it gets persisted in the backend.
func CheckAndSetEventFields(event AuditEvent, clock clockwork.Clock, uid utils.UID) error {
	if event.GetType() == "" {
		return trace.BadParameter("missing mandatory event type field")
	}
	if event.GetCode() == "" && event.GetType() != SessionPrintEvent {
		return trace.BadParameter("missing mandatory event code field for %v event", event.GetType())
	}
	if event.GetID() == "" && event.GetType() != SessionPrintEvent {
		event.SetID(uid.New())
	}
	if event.GetTime().IsZero() {
		event.SetTime(clock.Now().UTC().Round(time.Millisecond))
	}
	return nil
}

// DiscardStream returns a stream that discards all events
type DiscardStream struct {
}

// Write discards data
func (*DiscardStream) Write(p []byte) (n int, err error) {
	return len(p), nil
}

// Status returns a channel that always blocks
func (*DiscardStream) Status() <-chan StreamStatus {
	return nil
}

// Done returns channel closed when streamer is closed
// should be used to detect sending errors
func (*DiscardStream) Done() <-chan struct{} {
	return nil
}

// Close cancels and releases all resources associated
// with the stream without completing the stream,
// can be called multiple times
func (*DiscardStream) Close() error {
	return nil
}

// Complete does nothing
func (*DiscardStream) Complete(ctx context.Context) error {
	return nil
}

// EmitAuditEvent discards audit event
func (*DiscardStream) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	log.Debugf("Dicarding stream event: %v", event)
	return nil
}

// NewDiscardEmitter returns a no-op discard emitter
func NewDiscardEmitter() *DiscardEmitter {
	return &DiscardEmitter{}
}

// DiscardEmitter discards all events
type DiscardEmitter struct {
}

// EmitAuditEvent discards audit event
func (*DiscardEmitter) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	log.Debugf("Dicarding event: %v", event)
	return nil
}

// CreateAuditStream creates a stream that discards all events
func (*DiscardEmitter) CreateAuditStream(ctx context.Context, sid session.ID) (Stream, error) {
	return &DiscardStream{}, nil
}

// ResumeAuditStream resumes a stream that discards all events
func (*DiscardEmitter) ResumeAuditStream(ctx context.Context, sid session.ID) (Stream, error) {
	return &DiscardStream{}, nil
}

// NewLoggingEmitter returns an emitter that logs all events to the console
// with the info level
func NewLoggingEmitter() *LoggingEmitter {
	return &LoggingEmitter{}
}

// LoggingEmitter logs all events with info level
type LoggingEmitter struct {
}

// EmitAuditEvent logs audit event, skips session print events
// and session disk events, because they are very verbose
func (*LoggingEmitter) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	if event.GetType() == SessionDiskEvent || event.GetType() == SessionPrintEvent || event.GetType() == "" {
		return nil
	}
	data, err := utils.FastMarshal(event)
	if err != nil {
		return trace.Wrap(err)
	}

	var fields log.Fields
	err = utils.FastUnmarshal(data, &fields)
	if err != nil {
		return trace.Wrap(err)
	}
	fields[trace.Component] = teleport.Component(teleport.ComponentAuditLog)

	log.WithFields(fields).Infof(event.GetType())
	return nil
}

// NewMultiEmitter returns emitter that writes
// events to all emitters
func NewMultiEmitter(emitters ...Emitter) *MultiEmitter {
	return &MultiEmitter{
		emitters: emitters,
	}
}

// MultiEmitter writes audit events to multiple emitters
type MultiEmitter struct {
	emitters []Emitter
}

// EmitAuditEvent emits audit event to all emitters
func (m *MultiEmitter) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	var errors []error
	for i := range m.emitters {
		err := m.emitters[i].EmitAuditEvent(ctx, event)
		if err != nil {
			errors = append(errors, err)
		}
	}
	return trace.NewAggregate(errors...)
}

// StreamerAndEmitter combines streamer and emitter to create stream emitter
type StreamerAndEmitter struct {
	Streamer
	Emitter
}

// CheckingStreamerConfig provides parameters for streamer
type CheckingStreamerConfig struct {
	// Inner emits events to the underlying store
	Inner Streamer
	// Clock is a clock interface, used in tests
	Clock clockwork.Clock
	// UIDGenerator is unique ID generator
	UIDGenerator utils.UID
}

// NewCheckingStream wraps stream and makes sure event UIDs and timing are in place
func NewCheckingStream(stream Stream, clock clockwork.Clock) Stream {
	return &CheckingStream{
		stream:       stream,
		clock:        clock,
		uidGenerator: utils.NewRealUID(),
	}
}

// NewCheckingStreamer returns streamer that checks
// that all required fields are properly set
func NewCheckingStreamer(cfg CheckingStreamerConfig) (*CheckingStreamer, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	return &CheckingStreamer{
		CheckingStreamerConfig: cfg,
	}, nil
}

// CheckingStreamer ensures that event fields have been set properly
// and reports statistics for every wrapper
type CheckingStreamer struct {
	CheckingStreamerConfig
}

// CreateAuditStream creates audit event stream
func (s *CheckingStreamer) CreateAuditStream(ctx context.Context, sid session.ID) (Stream, error) {
	stream, err := s.Inner.CreateAuditStream(ctx, sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &CheckingStream{
		clock:        s.CheckingStreamerConfig.Clock,
		uidGenerator: s.CheckingStreamerConfig.UIDGenerator,
		stream:       stream,
	}, nil
}

// ResumeAuditStream resumes audit event stream
func (s *CheckingStreamer) ResumeAuditStream(ctx context.Context, sid session.ID, uploadID string) (Stream, error) {
	stream, err := s.Inner.ResumeAuditStream(ctx, sid, uploadID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &CheckingStream{
		clock:        s.CheckingStreamerConfig.Clock,
		uidGenerator: s.CheckingStreamerConfig.UIDGenerator,
		stream:       stream,
	}, nil
}

// CheckAndSetDefaults checks and sets default values
func (w *CheckingStreamerConfig) CheckAndSetDefaults() error {
	if w.Inner == nil {
		return trace.BadParameter("missing parameter Inner")
	}
	if w.Clock == nil {
		w.Clock = clockwork.NewRealClock()
	}
	if w.UIDGenerator == nil {
		w.UIDGenerator = utils.NewRealUID()
	}
	return nil
}

// CheckingStream verifies every event
type CheckingStream struct {
	stream       Stream
	clock        clockwork.Clock
	uidGenerator utils.UID
}

// Done returns channel closed when streamer is closed
// should be used to detect sending errors
func (s *CheckingStream) Done() <-chan struct{} {
	return s.stream.Done()
}

// Status returns channel receiving updates about stream status
// last event index that was uploaded and upload ID
func (s *CheckingStream) Status() <-chan StreamStatus {
	return s.stream.Status()
}

// Close cancels and releases all resources associated
// with the stream without completing the stream,
// can be called multiple times
func (s *CheckingStream) Close() error {
	return s.stream.Close()
}

// Complete closes the stream and marks it finalized
func (s *CheckingStream) Complete(ctx context.Context) error {
	return s.stream.Complete(ctx)
}

// EmitAuditEvent emits audit event
func (s *CheckingStream) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	if err := CheckAndSetEventFields(event, s.clock, s.uidGenerator); err != nil {
		log.WithError(err).Errorf("Failed to emit audit event %v(%v).", event.GetType(), event.GetCode())
		auditFailedEmit.Inc()
		return trace.Wrap(err)
	}
	if err := s.stream.EmitAuditEvent(ctx, event); err != nil {
		auditFailedEmit.Inc()
		log.WithError(err).Errorf("Failed to emit audit event %v(%v).", event.GetType(), event.GetCode())
		if event.GetType() == SessionEndEvent {
			debug.PrintStack()
			log.Errorf("STREAM ERR sesh end %v %v %v.", SessionEndEvent, event, err)
		}
		return trace.Wrap(err)
	}
	if event.GetType() == SessionEndEvent {
		debug.PrintStack()
		log.Errorf("STREAM OK sesh end %v %v.", SessionEndEvent, event)
	}
	return nil
}

// NewTeeStreamer returns a streamer that forwards non print event
// to emitter in addition to sending them to the stream
func NewTeeStreamer(streamer Streamer, emitter Emitter) *TeeStreamer {
	return &TeeStreamer{
		Emitter:  emitter,
		streamer: streamer,
	}
}

// CreateAuditStream creates audit event stream
func (t *TeeStreamer) CreateAuditStream(ctx context.Context, sid session.ID) (Stream, error) {
	stream, err := t.streamer.CreateAuditStream(ctx, sid)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &TeeStream{stream: stream, emitter: t.Emitter}, nil

}

// ResumeAuditStream resumes audit event stream
func (t *TeeStreamer) ResumeAuditStream(ctx context.Context, sid session.ID, uploadID string) (Stream, error) {
	stream, err := t.streamer.ResumeAuditStream(ctx, sid, uploadID)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &TeeStream{stream: stream, emitter: t.Emitter}, nil
}

// TeeStreamer creates streams that forwards non print events
// to emitter
type TeeStreamer struct {
	Emitter
	streamer Streamer
}

// TeeStream sends non print events to emitter
// in addition to the stream itself
type TeeStream struct {
	emitter Emitter
	stream  Stream
}

// Done returns channel closed when streamer is closed
// should be used to detect sending errors
func (t *TeeStream) Done() <-chan struct{} {
	return t.stream.Done()
}

// Status returns channel receiving updates about stream status
// last event index that was uploaded and upload ID
func (t *TeeStream) Status() <-chan StreamStatus {
	return t.stream.Status()
}

// Close cancels and releases all resources associated
// with the stream without completing the stream,
// can be called multiple times
func (t *TeeStream) Close() error {
	return t.stream.Close()
}

// Complete closes the stream and marks it finalized
func (t *TeeStream) Complete(ctx context.Context) error {
	return t.stream.Complete(ctx)
}

// EmitAuditEvent emits audit event
func (t *TeeStream) EmitAuditEvent(ctx context.Context, event AuditEvent) error {
	var errors []error
	if err := t.stream.EmitAuditEvent(ctx, event); err != nil {
		errors = append(errors, err)
	}
	// Forward non print and non disk events to emitter
	if event.GetType() != SessionDiskEvent && event.GetType() != SessionPrintEvent && event.GetType() != "" {
		if err := t.emitter.EmitAuditEvent(ctx, event); err != nil {
			errors = append(errors, err)
		}
	}
	return trace.NewAggregate(errors...)
}
