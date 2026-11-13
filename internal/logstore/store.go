// Package logstore writes metrics and request logs asynchronously to daily
// JSONL files and prints per-request console stats.
package logstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

// Options configures a Store.
type Options struct {
	Dir           string
	Location      *time.Location
	QueueSize     int
	MaxBodyBytes  int64
	StripImages   bool
	RetentionDays int
	Console       io.Writer // nil disables console stats
	ErrorLog      func(format string, args ...any)
	now           func() time.Time
}

type entry struct {
	rec *metrics.Record
	req *metrics.RequestLog
	end time.Time
}

// Store is an asynchronous, non-blocking log sink.
type Store struct {
	opts    Options
	ch      chan entry
	mu      sync.RWMutex
	closed  bool
	done    chan struct{}
	dropped atomic.Int64

	day         string
	metricsF    *dayFile
	requestsF   *dayFile
	dropBase    int64 // value of dropped when the current day started
	dropWritten int64 // drops last persisted for the current day
}

type dayFile struct {
	f *os.File
	w *bufio.Writer
}

// Sub-directories under the log root.
const (
	MetricsDir  = "metrics"
	RequestsDir = "requests"
	ReportsDir  = "reports"
)

// Open creates the directories and starts the writer goroutine.
func Open(opts Options) (*Store, error) {
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 10000
	}
	if opts.ErrorLog == nil {
		opts.ErrorLog = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "llmproxy: "+f+"\n", a...) }
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	for _, d := range []string{MetricsDir, RequestsDir, ReportsDir} {
		if err := os.MkdirAll(filepath.Join(opts.Dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{opts: opts, ch: make(chan entry, opts.QueueSize), done: make(chan struct{})}
	s.housekeep(s.today())
	go s.run()
	return s, nil
}

// Submit enqueues a record without blocking; when the queue is full the
// record is dropped and counted.
func (s *Store) Submit(rec *metrics.Record, req *metrics.RequestLog) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- entry{rec, req, s.opts.now()}:
	default:
		s.dropped.Add(1)
	}
}

// Dropped returns the total number of dropped records since start.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// Close stops accepting records, drains the queue and closes files.
func (s *Store) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	<-s.done
	return nil
}

func (s *Store) today() string { return s.opts.now().In(s.opts.Location).Format("2006-01-02") }

func (s *Store) run() {
	defer close(s.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-s.ch:
			if !ok {
				s.flush()
				s.closeDay()
				return
			}
			s.write(e)
			// Drain whatever is queued before flushing.
			for more := true; more; {
				select {
				case e, ok := <-s.ch:
					if !ok {
						s.flush()
						s.closeDay()
						return
					}
					s.write(e)
				default:
					more = false
				}
			}
			s.flush()
		case <-tick.C:
			if d := s.today(); s.day != "" && d != s.day {
				s.rotate(d)
			}
			s.flush()
		}
	}
}

func (s *Store) write(e entry) {
	day := e.end.In(s.opts.Location).Format("2006-01-02")
	if day != s.day {
		s.rotate(day)
	}
	if s.metricsF != nil {
		if b, err := json.Marshal(e.rec); err == nil {
			s.metricsF.w.Write(b)
			s.metricsF.w.WriteByte('\n')
		}
	}
	if s.requestsF != nil && e.req != nil {
		s.requestsF.w.Write(s.encodeRequest(e.req))
		s.requestsF.w.WriteByte('\n')
	}
	if s.opts.Console != nil {
		fmt.Fprintln(s.opts.Console, metrics.ConsoleLine(e.rec, e.end, s.opts.Location))
	}
}

func (s *Store) flush() {
	for _, f := range []*dayFile{s.metricsF, s.requestsF} {
		if f != nil {
			if err := f.w.Flush(); err != nil {
				s.opts.ErrorLog("flush log: %v", err)
			}
		}
	}
	s.writeDrops()
}

// writeDrops persists the number of records dropped on the current day.
func (s *Store) writeDrops() {
	if s.day == "" {
		return
	}
	n := s.dropped.Load() - s.dropBase
	if n == s.dropWritten {
		return
	}
	if err := os.WriteFile(DropsPath(s.opts.Dir, s.day), []byte(strconv.FormatInt(n, 10)+"\n"), 0o644); err != nil {
		s.opts.ErrorLog("write drops: %v", err)
		return
	}
	s.dropWritten = n
}

func (s *Store) closeDay() {
	for _, f := range []*dayFile{s.metricsF, s.requestsF} {
		if f != nil {
			f.w.Flush()
			f.f.Close()
		}
	}
	s.metricsF, s.requestsF = nil, nil
}

func (s *Store) rotate(day string) {
	s.flush()
	s.closeDay()
	if s.day != "" {
		s.dropBase = s.dropped.Load()
	}
	s.day = day
	s.dropWritten = ReadDrops(s.opts.Dir, day)
	s.dropBase -= s.dropWritten // continue counting after a restart
	var err error
	if s.metricsF, err = openDay(filepath.Join(s.opts.Dir, MetricsDir, day+".jsonl")); err != nil {
		s.opts.ErrorLog("open metrics log: %v", err)
	}
	if s.requestsF, err = openDay(filepath.Join(s.opts.Dir, RequestsDir, day+".jsonl")); err != nil {
		s.opts.ErrorLog("open request log: %v", err)
	}
	go s.housekeep(day)
}

func openDay(path string) (*dayFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &dayFile{f: f, w: bufio.NewWriterSize(f, 256<<10)}, nil
}

// DropsPath is the file holding the number of records dropped on day.
func DropsPath(dir, day string) string {
	return filepath.Join(dir, MetricsDir, day+".dropped")
}

// ReadDrops returns the number of records dropped on day (0 if unknown).
func ReadDrops(dir, day string) int64 {
	b, err := os.ReadFile(DropsPath(dir, day))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}
