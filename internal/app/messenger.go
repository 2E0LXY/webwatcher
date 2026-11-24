package app

import (
	"context"
	"log/slog"
	"sync"
)

type MsgKind string

const (
	KindLogin MsgKind = "login"
	KindAlert MsgKind = "alert"
)

// Message is a single outbound WhatsApp message. Text is the full plain-text
// rendering (log and Twilio free-form); Params are the body parameters for
// pre-approved templates (Meta, Twilio content templates).
type Message struct {
	Kind   MsgKind
	To     string // E.164
	Text   string
	Params []string
}

type Messenger interface {
	Send(ctx context.Context, m Message) error
	Name() string
}

// LogMessenger writes messages to the log. Development only: it logs magic links.
type LogMessenger struct {
	mu     sync.Mutex
	recent []Message
}

func (l *LogMessenger) Name() string { return "log" }

func (l *LogMessenger) Send(_ context.Context, m Message) error {
	// Dev only. Login links are logged on purpose; alert text holds page URLs, so it is not.
	if m.Kind == KindLogin {
		slog.Info("message (log messenger)", "kind", string(m.Kind), "to", MaskPhone(m.To), "text", m.Text)
	} else {
		slog.Info("message (log messenger)", "kind", string(m.Kind), "to", MaskPhone(m.To))
	}
	l.mu.Lock()
	l.recent = append(l.recent, m)
	if len(l.recent) > 20 {
		l.recent = l.recent[len(l.recent)-20:]
	}
	l.mu.Unlock()
	return nil
}

// Recent returns captured messages, newest first (for the /dev page).
func (l *LogMessenger) Recent() []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Message, len(l.recent))
	for i, m := range l.recent {
		out[len(out)-1-i] = m
	}
	return out
}
