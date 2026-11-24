package agent

import (
	"os"
	"path/filepath"
	"sync"
)

// DefaultLogCap is the size at which the log is rotated.
const DefaultLogCap = 2 << 20

// RotatingFile is a log writer with a simple size cap: when the file would
// pass max bytes it is renamed to path+".1" (replacing the previous one) and a
// new file started. At most about 2*max bytes are ever kept.
type RotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func NewRotatingFile(path string, max int64) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &RotatingFile{path: path, max: max}
	return r, r.open()
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.f.Close()
		os.Remove(r.path + ".1")
		if err := os.Rename(r.path, r.path+".1"); err != nil {
			os.Remove(r.path)
		}
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
