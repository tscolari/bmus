package store

import "sync"

// DefaultFeedBuffer is how many entries a Feed subscriber may fall behind
// by before it is dropped.
const DefaultFeedBuffer = 256

// Feed fans captain's log entries out to in-process subscribers (#72).
// The store's log hook feeds it (SetLogHook(ctx, feed.Publish, onErr)). Each
// subscriber gets a bounded channel; Publish never blocks, and drops a
// subscriber whose channel is full. A dropped subscriber catches up from
// the log itself.
type Feed struct {
	buf  int
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// NewFeed returns a Feed whose subscribers buffer buf entries; buf <= 0
// means DefaultFeedBuffer.
func NewFeed(buf int) *Feed {
	if buf <= 0 {
		buf = DefaultFeedBuffer
	}
	return &Feed{buf: buf, subs: map[*Subscription]struct{}{}}
}

// Subscription is one Feed subscriber.
type Subscription struct {
	feed *Feed
	c    chan LogEntry
}

// Subscribe returns a subscription to the entries published from now on.
func (f *Feed) Subscribe() *Subscription {
	sub := &Subscription{feed: f, c: make(chan LogEntry, f.buf)}
	f.mu.Lock()
	f.subs[sub] = struct{}{}
	f.mu.Unlock()
	return sub
}

// C returns the subscription's entries, in log order. It is closed when
// the subscriber falls behind (and is dropped) or calls Close; the
// entries buffered before that are still received first.
func (s *Subscription) C() <-chan LogEntry { return s.c }

// Close unsubscribes. It is safe to call more than once, and after a
// drop.
func (s *Subscription) Close() {
	s.feed.mu.Lock()
	s.feed.remove(s)
	s.feed.mu.Unlock()
}

// remove drops sub and closes its channel, if still subscribed. f.mu must
// be held.
func (f *Feed) remove(sub *Subscription) {
	if _, ok := f.subs[sub]; ok {
		delete(f.subs, sub)
		close(sub.c)
	}
}

// Publish hands entries to every subscriber without blocking. A
// subscriber without room for all of them is dropped.
func (f *Feed) Publish(entries []LogEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub := range f.subs {
		if cap(sub.c)-len(sub.c) < len(entries) {
			f.remove(sub)
			continue
		}
		for _, e := range entries {
			sub.c <- e
		}
	}
}
