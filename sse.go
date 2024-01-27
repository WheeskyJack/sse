// Package sse implements a http.Handler capable of delivering server-side events to clients.
package sse

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Stream is an HTTP event stream.
type Stream struct {
	requests        chan request
	channels        []chan message
	concurrencyChan chan struct{}

	reqChanSize         int
	msgChanSize         int
	concurrencyChanSize int

	timeoutToClient time.Duration

	Logger *log.Logger
}

// NewStream returns a new event stream that is ready to use.
// A Stream implements the http.Handler interface, through which clients can subscribe to new events.
// It also exposes Send and SendEvent methods, which can be used to notify all active listeners.
func NewStream(opts ...Option) *Stream {
	s := &Stream{
		reqChanSize:         10, // default channel size
		msgChanSize:         10, // default channel size
		concurrencyChanSize: 10, // default channel size
		Logger:              log.Default(),
		timeoutToClient:     10 * time.Second,
	}

	for _, o := range opts {
		o(s)
	}

	if s.timeoutToClient <= 0 {
		s.timeoutToClient = 10 * time.Second
	}
	if s.concurrencyChanSize <= 0 {
		s.concurrencyChanSize = 10
	}

	s.requests = make(chan request, s.reqChanSize)
	s.concurrencyChan = make(chan struct{}, s.concurrencyChanSize)

	go s.run()
	return s
}

// Option configures options for Stream
type Option func(s *Stream)

// WithReqChanSize sets size of requests channel.
func WithReqChanSize(size int) Option {
	return func(s *Stream) {
		s.reqChanSize = size
	}
}

// WithReqChanSize sets size of message channel.
func WithMsgChanSize(size int) Option {
	return func(s *Stream) {
		s.msgChanSize = size
	}
}

// WithLogger sets the logger.
func WithLogger(l *log.Logger) Option {
	return func(s *Stream) {
		s.Logger = l
	}
}

// WithConcurrencySize sets size of concurrency channel.
func WithConcurrencySize(size int) Option {
	return func(s *Stream) {
		s.concurrencyChanSize = size
	}
}

// WithTimeoutToClient sets timeout for clients.
func WithTimeoutToClient(timeout time.Duration) Option {
	return func(s *Stream) {
		s.timeoutToClient = timeout
	}
}

// Send sends a new event of type "message" to all listening clients.
// It is equivalent to a call to SendEvent with event == "message".
func (s *Stream) Send(ctx context.Context, data []byte) error {
	return s.SendEvent(ctx, "message", data)
}

// SendEvent sends a new event of given type to all listening clients.
func (s *Stream) SendEvent(ctx context.Context, event string, data []byte) error {
	select {
	case s.requests <- request{cmd: "notify", m: message{event: event, data: data}}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Stream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "server-side events not supported", http.StatusNotImplemented)
		return
	}

	messages := s.subscribe()
	defer func() {
		s.leave(messages)
	}()

	w.Header().Set("Content-Type", "text/event-stream")

	for m := range messages {
		var err error

		_, err = fmt.Fprintf(w, "event: %s\n", m.event)
		if err != nil {
			s.logf("connection lost")
			break
		}

		_, err = fmt.Fprintf(w, "data: %s\n\n", m.data)
		if err != nil {
			s.logf("connection lost")
			break
		}

		w.(http.Flusher).Flush()
	}
}

type request struct {
	cmd string       // one of "subscribe", "leave", "notify"
	c   chan message // only for cmd == "subscribe", "leave"
	m   message      // only for cmd == "notify"
}

type message struct {
	event string
	data  []byte
}

func (s *Stream) run() {
	for req := range s.requests {
		switch req.cmd {
		case "subscribe":
			s.channels = append(s.channels, req.c)
			s.logf("new listener: total %d", len(s.channels))
		case "leave":
			for i, c := range s.channels {
				if c == req.c {
					close(c)

					// Remove from list
					last := len(s.channels) - 1
					s.channels[i] = s.channels[last]
					s.channels = s.channels[:last]

					break
				}
			}
			s.logf("del listener: total %d", len(s.channels))
		case "notify":
			wg := new(sync.WaitGroup)

			for _, c := range s.channels {
				wg.Add(1)
				s.concurrencyChan <- struct{}{} // controls the number of go routines launched
				go func(cc chan message, mm message) {
					defer func() {
						<-s.concurrencyChan
						wg.Done()
					}()

					sendMsg(s, cc, mm)
				}(c, req.m)

			}

			wg.Wait() // wait till all go routines finish
		default:
			panic("unexpected request type")
		}
	}
}

func sendMsg(s *Stream, c chan message, m message) {
	newtimer := time.NewTimer(s.timeoutToClient)
	defer func() {
		if !newtimer.Stop() {
			<-newtimer.C
		}
	}()

	select {
	case c <- m:
		// message sent
	case <-newtimer.C:
		s.logf("message dropped")
	}
}

func (s *Stream) subscribe() chan message {
	c := make(chan message, s.msgChanSize)
	s.requests <- request{cmd: "subscribe", c: c}
	return c
}

func (s *Stream) leave(c chan message) {
	s.requests <- request{cmd: "leave", c: c}
}

func (s *Stream) logf(format string, v ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, v...)
	}
}

func (s *Stream) LeaveAll() {
	var delChan []chan message
	delChan = append(delChan, s.channels...)
	for _, c := range delChan {
		s.leave(c)
	}
}
