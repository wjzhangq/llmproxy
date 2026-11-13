package logstore

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var housekeepMu sync.Mutex

// housekeep gzips day files older than today and removes files past the
// retention period.
func (s *Store) housekeep(today string) {
	housekeepMu.Lock()
	defer housekeepMu.Unlock()
	t, err := time.ParseInLocation("2006-01-02", today, s.opts.Location)
	if err != nil {
		return
	}
	cutoff := ""
	if s.opts.RetentionDays > 0 {
		cutoff = t.AddDate(0, 0, -s.opts.RetentionDays).Format("2006-01-02")
	}
	for _, sub := range []string{MetricsDir, RequestsDir} {
		dir := filepath.Join(s.opts.Dir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if len(name) < 10 {
				continue
			}
			day := name[:10]
			if _, err := time.Parse("2006-01-02", day); err != nil {
				continue
			}
			path := filepath.Join(dir, name)
			if cutoff != "" && day < cutoff {
				if err := os.Remove(path); err != nil {
					s.opts.ErrorLog("remove expired log %s: %v", path, err)
				}
				continue
			}
			if day < today && strings.HasSuffix(name, ".jsonl") {
				if err := gzipFile(path); err != nil {
					s.opts.ErrorLog("compress %s: %v", path, err)
				}
			}
		}
	}
}

func gzipFile(path string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := path + ".gz.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path+".gz"); err != nil {
		return err
	}
	return os.Remove(path)
}

// OpenDay opens a day's JSONL file in sub, transparently reading the gzipped
// variant when the plain file no longer exists.
func OpenDay(dir, sub, day string) (io.ReadCloser, error) {
	base := filepath.Join(dir, sub, day+".jsonl")
	f, err := os.Open(base)
	if err == nil {
		return f, nil
	}
	gf, gerr := os.Open(base + ".gz")
	if gerr != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(gf)
	if err != nil {
		gf.Close()
		return nil, err
	}
	return &gzReadCloser{zr, gf}, nil
}

type gzReadCloser struct {
	*gzip.Reader
	f *os.File
}

func (g *gzReadCloser) Close() error {
	g.Reader.Close()
	return g.f.Close()
}
