package daemon

import (
	"fmt"
	"strings"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
)

// firstLineLen caps the `held` listing's body preview.
const firstLineLen = 80

// inboundPolicy decides whether m from cert is accepted, held or refused (own person always accepted).
func (d *Daemon) inboundPolicy(cert identity.Cert, m message.Message) (message.State, string) {
	if cert.Person == d.opts.Person || d.opts.PolicyFor == nil {
		return message.StateDelivered, ""
	}
	switch d.opts.PolicyFor(cert.Person) {
	case config.Hold:
		return message.StateHeld, heldNote(d.opts.Person)
	case config.Refuse:
		return message.StateRefused, refusedNote(d.opts.Person, cert.Person)
	default: // config.Accept
		return message.StateDelivered, ""
	}
}

// heldNote is the receipt note for a message held awaiting self's approval.
func heldNote(self string) string { return fmt.Sprintf("held until %s approves it", self) }

// refusedNote is the receipt note for a message from sender refused by self's policy.
func refusedNote(self, sender string) string {
	return fmt.Sprintf("%s doesn't accept messages from %s", self, sender)
}

// HeldArgs is the "held" op's arguments (none).
type HeldArgs struct{}

// HeldMessage summarizes one held message for the "held" op and the `held` command.
type HeldMessage struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	First string `json:"first"`
}

// HeldResult answers a "held" op: every held message, oldest first.
type HeldResult struct {
	Messages []HeldMessage `json:"messages"`
}

// held handles the "held" op.
func (d *Daemon) held() (HeldResult, error) {
	hs, err := d.opts.Store.Held()
	if err != nil {
		return HeldResult{}, err
	}
	res := HeldResult{Messages: make([]HeldMessage, len(hs))}
	for i, h := range hs {
		res.Messages[i] = HeldMessage{ID: h.Message.ID, From: h.Message.From, To: h.Message.To, First: firstLine(h.Message.Body)}
	}
	return res, nil
}

// firstLine is body's first line, defanged and capped to firstLineLen runes.
func firstLine(body string) string {
	line, _, _ := strings.Cut(message.Defang(body), "\n")
	r := []rune(line)
	if len(r) > firstLineLen {
		r = r[:firstLineLen]
	}
	return string(r)
}

// ApproveArgs is the "approve" op's arguments: exactly one of ID or Person.
type ApproveArgs struct {
	ID     string `json:"id,omitempty"`
	Person string `json:"person,omitempty"`
}

// ApproveResult answers an "approve" op: how many held messages were released.
type ApproveResult struct {
	Released int `json:"released"`
}

// approve handles the "approve" op.
func (d *Daemon) approve(a ApproveArgs) (ApproveResult, error) {
	if a.Person != "" {
		return d.approvePerson(a.Person)
	}
	return d.approveID(a.ID)
}

// approveID releases the one held message id, if it's still held (a no-op otherwise, e.g. already
// delivered or absent), and delivers it. ReleaseHeld's own transaction is the only guard needed.
func (d *Daemon) approveID(id string) (ApproveResult, error) {
	msgs, err := d.opts.Store.ReleaseHeld([]string{id})
	if err != nil {
		return ApproveResult{}, err
	}
	d.deliverReleasedAll(msgs)
	return ApproveResult{Released: len(msgs)}, nil
}

// maxPolicyFlushPasses bounds the straggler-release loop after a policy change: policyMu closes the
// race for any single admission, but a loop is cheap insurance and stops as soon as a pass finds none.
const maxPolicyFlushPasses = 5

// approvePerson sets person's policy to accept, then releases and delivers their held mail, looping
// until none remain so a message admitted while the change was publishing can't end up stranded.
func (d *Daemon) approvePerson(person string) (ApproveResult, error) {
	if err := d.publishPolicy(person, config.Accept); err != nil {
		return ApproveResult{}, err
	}
	var released int
	for range maxPolicyFlushPasses {
		ids, err := d.heldIDsFrom(person)
		if err != nil {
			return ApproveResult{Released: released}, err
		}
		if len(ids) == 0 {
			break
		}
		msgs, err := d.opts.Store.ReleaseHeld(ids)
		if err != nil {
			return ApproveResult{Released: released}, err
		}
		d.deliverReleasedAll(msgs)
		released += len(msgs)
	}
	return ApproveResult{Released: released}, nil
}

// heldIDsFrom returns the ids of every currently held message from person.
func (d *Daemon) heldIDsFrom(person string) ([]string, error) {
	all, err := d.opts.Store.Held()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, h := range all {
		if from, err := message.ParseAddress(h.Message.From); err == nil && from.Person == person {
			ids = append(ids, h.Message.ID)
		}
	}
	return ids, nil
}

// publishPolicy sets person's policy and invalidates the policy cache under policyMu's write lock, so
// it can't return until every admission already past the policy read has also finished storing.
func (d *Daemon) publishPolicy(person string, p config.Policy) error {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	if err := d.setPersonPolicy(person, p); err != nil {
		return err
	}
	if d.opts.InvalidatePolicy != nil {
		d.opts.InvalidatePolicy()
	}
	return nil
}

// deliverReleasedAll delivers each released message to its current local session, in the background;
// a message whose session isn't resolvable just stays unread and surfaces via Pending once it's back.
func (d *Daemon) deliverReleasedAll(msgs []message.Message) {
	for _, m := range msgs {
		to, err := message.ParseAddress(m.To)
		if err != nil {
			continue
		}
		tg, err := d.localSession(to)
		if err != nil || tg == nil {
			continue
		}
		mm := m
		d.safeGo(func() { d.deliver(tg.key, tg.name, []message.Message{mm}, mm.Wake) })
	}
}

// RefusePersonArgs is the "refuse_person" op's arguments.
type RefusePersonArgs struct {
	Person string `json:"person"`
}

// RefusePersonResult answers a "refuse_person" op: how many held messages were dropped.
type RefusePersonResult struct {
	Dropped int `json:"dropped"`
}

// refusePerson handles the "refuse_person" op: sets person's policy to refuse, then drops their held
// mail, repeating until none remain so a message admitted while the change was publishing can't survive.
func (d *Daemon) refusePerson(a RefusePersonArgs) (RefusePersonResult, error) {
	if err := d.publishPolicy(a.Person, config.Refuse); err != nil {
		return RefusePersonResult{}, err
	}
	var dropped int
	for range maxPolicyFlushPasses {
		ids, err := d.heldIDsFrom(a.Person)
		if err != nil {
			return RefusePersonResult{Dropped: dropped}, err
		}
		if len(ids) == 0 {
			break
		}
		if err := d.opts.Store.Drop(ids); err != nil {
			return RefusePersonResult{Dropped: dropped}, err
		}
		dropped += len(ids)
	}
	return RefusePersonResult{Dropped: dropped}, nil
}

// setPersonPolicy sets person's People override on disk, serialized against every other config writer.
func (d *Daemon) setPersonPolicy(person string, p config.Policy) error {
	return config.Update(d.opts.ConfigPath, func(cfg *config.Config) error {
		if cfg.People == nil {
			cfg.People = map[string]config.Policy{}
		}
		cfg.People[person] = p
		return nil
	})
}
