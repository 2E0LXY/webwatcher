package agent

import (
	"context"
	"errors"
)

// Rendered is what a browser load produced.
type Rendered struct {
	HTML     string // document.documentElement.outerHTML after the page settled
	FinalURL string
	Status   int // HTTP status of the main document, 0 if unknown
	LoadMS   int64
	IdleMS   int64
}

// LoadError is a load that failed before a page could be read.
type LoadError struct {
	Class string // dns, connect, tls, timeout, nav_error, other
	Err   error
}

func (e *LoadError) Error() string { return "load failed (" + e.Class + "): " + e.Err.Error() }
func (e *LoadError) Unwrap() error { return e.Err }

// Browser loads one URL at a time in a real browser. Implementations must close
// every tab they open and never take screenshots.
type Browser interface {
	Load(ctx context.Context, rawURL string) (*Rendered, error)
	Close() error
}

// errRefused is returned for URLs the agent will not load.
var errRefused = errors.New("refused: only http and https URLs may be loaded")
