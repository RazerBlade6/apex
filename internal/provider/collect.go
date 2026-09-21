package provider

import (
	"context"
	"strings"
)

// Collect runs a streaming request to completion and returns the whole text.
//
// Every caller of Stream that does not render tokens as they arrive needs
// exactly this loop, and getting it wrong is easy in one specific way: the
// channel must be drained to close even after an error arrives, or the
// adapter's sending goroutine stays parked on its last send until the
// context is cancelled. Digest generation is the first such caller — a digest
// is written to the database, not to a terminal — and M6's chat view will be
// the first that genuinely wants the deltas.
//
// The first error wins. A stream that produced text and then failed returns
// both, so a caller can log what arrived before the failure; nothing here
// treats partial text as success.
func Collect(ctx context.Context, p Provider, req Request) (string, Usage, error) {
	events, err := p.Stream(ctx, req)
	if err != nil {
		return "", Usage{}, err
	}

	var (
		body  strings.Builder
		usage Usage
		first error
	)
	for ev := range events {
		switch ev.Type {
		case EventTextDelta:
			body.WriteString(ev.Text)
		case EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case EventError:
			if first == nil {
				first = ev.Err
			}
		}
	}
	return body.String(), usage, first
}
