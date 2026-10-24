package daemon

import (
	"fmt"
	"strings"

	"github.com/purnachand-jaddu/agentnet/config"
	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
)

// firstLineLen caps the `held` listing's body preview.
const firstLineLen = 80

// inboundPolicy decides whether m from cert is accepted, held or refused. Own person is always accepted;
// anyone else's is decided by PolicyFor (nil accepts everyone).
func (d *Daemon) inboundPolicy(cert identity.Cert, m message.Message) (message.State, string) {
	if cert.Person == d.opts.Person || d.opts.PolicyFor == nil {
		return message.StateDelivered, ""
	}
	switch d.opts.PolicyFor(cert.Person) {
	case config.Hold:
		return message.StateHeld, heldNote(cert.Person)
	case config.Refuse:
		return message.StateRefused, refusedNote(d.opts.Person, cert.Person)
	default: // config.Accept
		return message.StateDelivered, ""
	}
}

// heldNote is the receipt note for a message held awaiting approval of its sender's person.
func heldNote(person string) string { return fmt.Sprintf("held until %s approves it", person) }

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

// approveID releases the one held message id, if it's still held, and delivers it.
func (d *Daemon) approveID(id string) (ApproveResult, error) {
	m, mailbox, err := d.opts.Store.MessageByID(id)
	if err != nil || m == nil {
		return ApproveResult{}, err
	}
	if err := d.opts.Store.Release([]string{id}); err != nil {
		return ApproveResult{}, err
	}
	d.deliverReleased(mailbox, *m)
	return ApproveResult{Released: 1}, nil
}

// approvePerson releases every held message from person and sets its policy to accept.
func (d *Daemon) approvePerson(person string) (ApproveResult, error) {
	all, err := d.opts.Store.Held()
	if err != nil {
		return ApproveResult{}, err
	}
	var release []store.Held
	for _, h := range all {
		if from, err := message.ParseAddress(h.Message.From); err == nil && from.Person == person {
			release = append(release, h)
		}
	}
	if err := d.setPersonPolicy(person, config.Accept); err != nil {
		return ApproveResult{}, err
	}
	if len(release) == 0 {
		return ApproveResult{}, nil
	}
	ids := make([]string, len(release))
	for i, h := range release {
		ids[i] = h.Message.ID
	}
	if err := d.opts.Store.Release(ids); err != nil {
		return ApproveResult{}, err
	}
	for _, h := range release {
		d.deliverReleased(h.Mailbox, h.Message)
	}
	return ApproveResult{Released: len(release)}, nil
}

// deliverReleased delivers a just-released message to its mailbox session, in the background.
func (d *Daemon) deliverReleased(mailbox string, m message.Message) {
	name := m.To
	if to, err := message.ParseAddress(m.To); err == nil {
		name = to.Session
	}
	d.safeGo(func() { d.deliver(mailbox, name, []message.Message{m}, m.Wake) })
}

// RefusePersonArgs is the "refuse_person" op's arguments.
type RefusePersonArgs struct {
	Person string `json:"person"`
}

// RefusePersonResult answers a "refuse_person" op: how many held messages were dropped.
type RefusePersonResult struct {
	Dropped int `json:"dropped"`
}

// refusePerson handles the "refuse_person" op: sets person's policy to refuse and drops their held mail.
func (d *Daemon) refusePerson(a RefusePersonArgs) (RefusePersonResult, error) {
	all, err := d.opts.Store.Held()
	if err != nil {
		return RefusePersonResult{}, err
	}
	var ids []string
	for _, h := range all {
		if from, err := message.ParseAddress(h.Message.From); err == nil && from.Person == a.Person {
			ids = append(ids, h.Message.ID)
		}
	}
	if err := d.setPersonPolicy(a.Person, config.Refuse); err != nil {
		return RefusePersonResult{}, err
	}
	if len(ids) == 0 {
		return RefusePersonResult{}, nil
	}
	if err := d.opts.Store.Drop(ids); err != nil {
		return RefusePersonResult{}, err
	}
	return RefusePersonResult{Dropped: len(ids)}, nil
}

// setPersonPolicy sets person's People override on disk via config.Save.
func (d *Daemon) setPersonPolicy(person string, p config.Policy) error {
	cfg, err := config.Load(d.opts.ConfigPath)
	if err != nil {
		return err
	}
	if cfg.People == nil {
		cfg.People = map[string]config.Policy{}
	}
	cfg.People[person] = p
	return cfg.Save(d.opts.ConfigPath)
}
