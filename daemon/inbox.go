package daemon

import (
	"context"
	"time"

	"github.com/purnachand-jaddu/agentnet/message"
)

// maxWaitSecs caps how long an "inbox" long-poll blocks.
const maxWaitSecs = 600

// InboxArgs is the "inbox" and "unread" ops' arguments.
type InboxArgs struct {
	Key      string `json:"key"`
	WaitSecs int    `json:"wait_secs"`
	Peek     bool   `json:"peek"`
}

// InboxResult answers an "inbox" op: the delivered ids and the agent-framed text.
type InboxResult struct {
	IDs  []string `json:"ids"`
	Text string   `json:"text"`
}

// UnreadResult answers an "unread" op.
type UnreadResult struct {
	Count int      `json:"count"`
	From  []string `json:"from"`
}

// DeliveredArgs is the "delivered" op's arguments: the session confirming it has ids.
type DeliveredArgs struct {
	Key string   `json:"key"`
	IDs []string `json:"ids"`
}

// inbox handles the "inbox" op: long-polls up to wait_secs (capped at maxWaitSecs) for mail at key.
func (d *Daemon) inbox(ctx context.Context, a InboxArgs) (InboxResult, error) {
	wait := a.WaitSecs
	if wait > maxWaitSecs {
		wait = maxWaitSecs
	}
	if wait < 0 {
		wait = 0
	}
	deadline := d.opts.Now().Add(time.Duration(wait) * time.Second)

	for {
		msgs, err := d.pending(a.Key)
		if err != nil {
			return InboxResult{}, err
		}
		if len(msgs) > 0 {
			return d.inboxResult(a, msgs)
		}
		remaining := deadline.Sub(d.opts.Now())
		if remaining <= 0 {
			return InboxResult{}, nil
		}

		d.mu.Lock()
		d.waiting[a.Key]++
		arrived := d.arrived
		d.mu.Unlock()

		timer := time.NewTimer(remaining)
		select {
		case <-arrived:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()

		d.mu.Lock()
		d.waiting[a.Key]--
		if d.waiting[a.Key] <= 0 {
			delete(d.waiting, a.Key)
		}
		d.mu.Unlock()

		if ctx.Err() != nil {
			return InboxResult{}, nil
		}
	}
}

// inboxResult marks msgs read (unless a.Peek) and formats them for the agent.
func (d *Daemon) inboxResult(a InboxArgs, msgs []message.Message) (InboxResult, error) {
	ids := idsOf(msgs)
	if !a.Peek {
		if err := d.opts.Store.MarkRead(ids, d.opts.Now()); err != nil {
			return InboxResult{}, err
		}
	}
	return InboxResult{IDs: ids, Text: message.ForAgent(msgs)}, nil
}

// unread handles the "unread" op.
func (d *Daemon) unread(a InboxArgs) (UnreadResult, error) {
	msgs, err := d.pending(a.Key)
	if err != nil {
		return UnreadResult{}, err
	}
	res := UnreadResult{Count: len(msgs)}
	for _, m := range msgs {
		res.From = append(res.From, m.From)
	}
	return res, nil
}

// delivered handles the "delivered" op: the ids are read and no longer in-flight.
func (d *Daemon) delivered(a DeliveredArgs) error {
	if err := d.opts.Store.MarkRead(a.IDs, d.opts.Now()); err != nil {
		return err
	}
	d.mu.Lock()
	for _, id := range a.IDs {
		delete(d.inFlight, id)
	}
	d.mu.Unlock()
	return nil
}
