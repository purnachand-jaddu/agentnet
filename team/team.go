// Package team is this machine's view of its agentnet network: the founder key, its own cert and every member and block.
package team

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/purnachand-jaddu/agentnet/identity"
	"github.com/purnachand-jaddu/agentnet/message"
	"github.com/purnachand-jaddu/agentnet/store"
)

// ErrPersonTaken means another key already holds that person name.
var ErrPersonTaken = errors.New("that person name is already held by another machine")

// ErrOtherNetwork means this store already belongs to a different network; the CLI names where to look.
var ErrOtherNetwork = errors.New("team: already in another network")

const (
	nsTeam  = "team"
	nsCert  = "cert"
	nsBlock = "block"
)

// State is this machine's view of the team; its methods are safe for concurrent use, direct map access is not.
type State struct {
	mu      sync.RWMutex
	Founder ed25519.PublicKey
	Self    identity.Cert
	Members map[string]identity.Cert  // base64url key -> cert
	Blocks  map[string]identity.Block // base64url key -> block
}

// keyB64 is a cert or block's Key as the map key used to index Members and Blocks.
func keyB64(k []byte) string { return base64.RawURLEncoding.EncodeToString(k) }

// Load reads this store's team state, returning (nil, nil) when it hasn't joined a team yet.
func Load(s store.Store) (*State, error) {
	fb, err := s.Get(nsTeam, "founder")
	if err != nil {
		return nil, err
	}
	if fb == nil {
		return nil, nil
	}
	founder := ed25519.PublicKey(fb)

	st := &State{Founder: founder, Members: map[string]identity.Cert{}, Blocks: map[string]identity.Block{}}

	if selfB, err := s.Get(nsTeam, "self"); err != nil {
		return nil, err
	} else if selfB != nil {
		if err := json.Unmarshal(selfB, &st.Self); err != nil {
			return nil, err
		}
	}

	certs, err := s.List(nsCert)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for k, b := range certs {
		var c identity.Cert
		if err := json.Unmarshal(b, &c); err != nil {
			continue // drop an unreadable stored cert
		}
		if err := c.Verify(founder, now); err != nil {
			continue // drop a cert that no longer verifies against the founder
		}
		st.Members[k] = c
	}

	blocks, err := s.List(nsBlock)
	if err != nil {
		return nil, err
	}
	for k, b := range blocks {
		var blk identity.Block
		if err := json.Unmarshal(b, &blk); err != nil {
			continue
		}
		if err := blk.Verify(founder); err != nil {
			continue
		}
		st.Blocks[k] = blk
	}
	return st, nil
}

// save writes st's self cert, members and blocks to s, dropping any stored cert for a now-blocked key, and
// writes the founder key LAST: Load treats its presence as "this store belongs to a team", so a save that
// fails partway through must never leave it set — that would read back as a team with no self cert/members.
func (st *State) save(s store.Store) error {
	selfB, err := json.Marshal(st.Self)
	if err != nil {
		return err
	}
	if err := s.Put(nsTeam, "self", selfB); err != nil {
		return err
	}
	for k, c := range st.Members {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := s.Put(nsCert, k, b); err != nil {
			return err
		}
	}
	for k, blk := range st.Blocks {
		b, err := json.Marshal(blk)
		if err != nil {
			return err
		}
		if err := s.Put(nsBlock, k, b); err != nil {
			return err
		}
		if err := s.Delete(nsCert, k); err != nil { // a blocked key is never stored as a live member
			return err
		}
	}
	return s.Put(nsTeam, "founder", st.Founder)
}

// Create makes s the founder store of a new network: an admin cert for person@machine, signed by founder.
func Create(s store.Store, keys identity.Keys, founder ed25519.PrivateKey, person, machine string, now time.Time) (*State, error) {
	if !message.Slug(person) || !message.Slug(machine) {
		return nil, errors.New("team: person and machine names must use lowercase letters, digits and -")
	}
	founderPub := founder.Public().(ed25519.PublicKey)
	self := identity.Cert{
		V: 1, Team: identity.TeamID(founderPub), Key: keys.Public(), Enc: keys.EncPublic(),
		Person: person, Machine: machine, Admin: true, Issued: now.Unix(),
	}
	self.Sign(founder)

	st := &State{Founder: founderPub, Self: self, Members: map[string]identity.Cert{keyB64(self.Key): self}, Blocks: map[string]identity.Block{}}
	if err := st.save(s); err != nil {
		return nil, err
	}
	return st, nil
}

// NewRequest builds a join request for person@machine from keys, with a fresh random nonce.
func NewRequest(keys identity.Keys, person, machine string) identity.JoinRequest {
	nonce := make([]byte, 16)
	rand.Read(nonce) // crypto/rand.Read never returns an error
	return identity.JoinRequest{V: 1, Person: person, Machine: machine, Key: keys.Public(), Enc: keys.EncPublic(), Nonce: nonce}
}

// bundle builds the identity.Bundle this founder hands back for cert, from st's current members and blocks.
func (st *State) bundle(founderPub ed25519.PublicKey, cert identity.Cert) identity.Bundle {
	members := make([]identity.Cert, 0, len(st.Members))
	for _, c := range st.Members {
		members = append(members, c)
	}
	blocks := make([]identity.Block, 0, len(st.Blocks))
	for _, b := range st.Blocks {
		blocks = append(blocks, b)
	}
	return identity.Bundle{V: 1, Founder: founderPub, Cert: cert, Members: members, Blocks: blocks}
}

// Admit signs a cert for req and returns a bundle with the whole team. Admitting the same request
// (same key, person and machine) again is idempotent: it hands back the existing cert rather than erroring,
// so a lost bundle can be re-issued. ErrPersonTaken when another key already holds req.Person and samePerson is false.
func Admit(s store.Store, founder ed25519.PrivateKey, req identity.JoinRequest, samePerson bool, now time.Time) (identity.Bundle, error) {
	founderPub := founder.Public().(ed25519.PublicKey)
	st, err := Load(s)
	if err != nil {
		return identity.Bundle{}, err
	}
	if st == nil {
		return identity.Bundle{}, errors.New("team: no network on this machine")
	}
	if !st.Founder.Equal(founderPub) {
		return identity.Bundle{}, errors.New("team: that founder key does not match this network")
	}

	reqKey := keyB64(req.Key)
	if existing, ok := st.Members[reqKey]; ok {
		if existing.Person == req.Person && existing.Machine == req.Machine {
			return st.bundle(founderPub, existing), nil
		}
		return identity.Bundle{}, fmt.Errorf("team: %s already has a certificate for a different identity", req.Machine)
	}
	if _, blocked := st.Blocks[reqKey]; blocked {
		return identity.Bundle{}, errors.New("team: that key is blocked")
	}
	if !samePerson {
		for _, c := range st.Members {
			if c.Person == req.Person {
				return identity.Bundle{}, ErrPersonTaken
			}
		}
	}

	cert := identity.Cert{
		V: 1, Team: identity.TeamID(founderPub), Key: req.Key, Enc: req.Enc,
		Person: req.Person, Machine: req.Machine, Issued: now.Unix(),
	}
	cert.Sign(founder)

	st.Merge([]identity.Cert{cert}, nil, now)
	if err := st.save(s); err != nil {
		return identity.Bundle{}, err
	}
	return st.bundle(founderPub, cert), nil
}

// Accept verifies bundle b against b.Founder and this machine's keys, then merges it into s.
// The caller must first have a person compare TeamCode(b.Founder) with the founder's.
func Accept(s store.Store, keys identity.Keys, b identity.Bundle, now time.Time) (*State, error) {
	founderPub := ed25519.PublicKey(b.Founder)
	if err := b.Cert.Verify(founderPub, now); err != nil {
		return nil, fmt.Errorf("team: bundle cert: %w", err)
	}
	if !ed25519.PublicKey(b.Cert.Key).Equal(keys.Public()) || !bytes.Equal(b.Cert.Enc, keys.EncPublic()) {
		return nil, errors.New("team: this bundle's cert is not for this machine's keys")
	}

	existing, err := Load(s)
	if err != nil {
		return nil, err
	}
	if existing != nil && !existing.Founder.Equal(founderPub) {
		return nil, ErrOtherNetwork
	}

	st := existing
	if st == nil {
		st = &State{Founder: founderPub, Members: map[string]identity.Cert{}, Blocks: map[string]identity.Block{}}
	}
	st.Self = b.Cert
	certs := append([]identity.Cert{b.Cert}, b.Members...)
	st.Merge(certs, b.Blocks, now)
	if err := st.save(s); err != nil {
		return nil, err
	}
	return st, nil
}

// Verify checks c's founder signature and that c's key isn't blocked.
func (st *State) Verify(c identity.Cert, now time.Time) error {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if err := c.Verify(st.Founder, now); err != nil {
		return err
	}
	if _, blocked := st.Blocks[keyB64(c.Key)]; blocked {
		return fmt.Errorf("team: %s is blocked", c.Machine)
	}
	return nil
}

// Merge folds certs and blocks into st: only founder-verified entries survive, the later-Issued cert wins
// per key, and a block removes that key from Members. It reports whether st changed.
func (st *State) Merge(certs []identity.Cert, blocks []identity.Block, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	changed := false
	for _, c := range certs {
		if err := c.Verify(st.Founder, now); err != nil {
			continue
		}
		k := keyB64(c.Key)
		if _, blocked := st.Blocks[k]; blocked {
			continue // a block always wins over a gossiped cert, however fresh
		}
		if existing, ok := st.Members[k]; !ok || c.Issued > existing.Issued {
			st.Members[k] = c
			changed = true
		}
	}
	for _, b := range blocks {
		if err := b.Verify(st.Founder); err != nil {
			continue
		}
		k := keyB64(b.Key)
		if _, ok := st.Blocks[k]; !ok {
			st.Blocks[k] = b
			changed = true
		}
		if _, ok := st.Members[k]; ok {
			delete(st.Members, k)
			changed = true
		}
	}
	return changed
}

// ByPerson returns every member cert for person.
func (st *State) ByPerson(person string) []identity.Cert {
	st.mu.RLock()
	defer st.mu.RUnlock()
	var out []identity.Cert
	for _, c := range st.Members {
		if c.Person == person {
			out = append(out, c)
		}
	}
	return out
}

// Persist saves st to s, founder last.
func (st *State) Persist(s store.Store) error {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.save(s)
}

// CertFor returns the member cert for key, if any.
func (st *State) CertFor(key []byte) (identity.Cert, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	c, ok := st.Members[keyB64(key)]
	return c, ok
}

// MembersSnapshot returns a copy of every member cert.
func (st *State) MembersSnapshot() []identity.Cert {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]identity.Cert, 0, len(st.Members))
	for _, c := range st.Members {
		out = append(out, c)
	}
	return out
}

// BlocksSnapshot returns a copy of every block.
func (st *State) BlocksSnapshot() []identity.Block {
	st.mu.RLock()
	defer st.mu.RUnlock()
	out := make([]identity.Block, 0, len(st.Blocks))
	for _, b := range st.Blocks {
		out = append(out, b)
	}
	return out
}
