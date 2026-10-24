package daemon

import (
	"time"

	"github.com/purnachand-jaddu/agentnet/adapter"
	"github.com/purnachand-jaddu/agentnet/message"
)

// turnWatchInterval paces the turn-watch poll; a package var so tests can shorten it.
var turnWatchInterval = 2 * time.Second

// startTurnWatch starts (at most once per key) a poller that redelivers sess's wake mail once its running turn ends.
func (d *Daemon) startTurnWatch(key string, sess adapter.Session, a adapter.Adapter) {
	tw, ok := a.(adapter.TurnWatcher)
	if !ok {
		return
	}
	if running, known := tw.TurnRunning(sess); !known || !running {
		return
	}
	d.mu.Lock()
	if d.watching[key] {
		d.mu.Unlock()
		return
	}
	d.watching[key] = true
	d.mu.Unlock()

	d.safeGo(func() { d.runTurnWatch(key, tw) })
}

// runTurnWatch polls tw every turnWatchInterval, stopping once key is gone, has no unread wake mail, or its turn has ended (then redelivering once).
func (d *Daemon) runTurnWatch(key string, tw adapter.TurnWatcher) {
	defer func() {
		d.mu.Lock()
		delete(d.watching, key)
		d.mu.Unlock()
	}()
	for {
		time.Sleep(turnWatchInterval)

		d.mu.Lock()
		l, live := d.live[key]
		var sess adapter.Session
		var name string
		if live {
			sess, name = l.s, l.name
		}
		d.mu.Unlock()
		if !live {
			return
		}

		msgs, err := d.pending(key)
		if err != nil {
			d.opts.Log("daemon: turn watch pending %s: %v", key, err)
			return
		}
		wake := wakeOnly(msgs)
		if len(wake) == 0 {
			return
		}

		if running, known := tw.TurnRunning(sess); known && running {
			continue
		}
		d.deliver(key, name, wake, true)
		return
	}
}

// wakeOnly returns the subset of msgs with Wake set.
func wakeOnly(msgs []message.Message) []message.Message {
	var out []message.Message
	for _, m := range msgs {
		if m.Wake {
			out = append(out, m)
		}
	}
	return out
}
