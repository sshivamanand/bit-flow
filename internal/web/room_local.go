package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/store"
)

// Local is the room seen from inside this process: the desktop app joins as a
// member with no WebSocket, so phones and browsers show up in its device list
// and can send to it with its pairing code. Files it receives are written
// straight into a folder (never overwriting), not downloaded as a zip.
type Local struct {
	rm *room
	m  *member

	mu    sync.Mutex
	peers []RoomPeer
	xfers map[string]*LocalXfer
}

// RoomPeer is another member of the room.
type RoomPeer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Paired bool   `json:"paired"` // no code needed to send to them
	Online bool   `json:"online"`
}

// RoomFile is one file of a room transfer; Name may include folders ("Trip/a.jpg").
type RoomFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// LocalXfer is a room transfer this process sends or receives.
type LocalXfer struct {
	ID       string
	Out      bool // sending
	Peer     string
	PeerName string
	Files    []RoomFile
	Total    int64
	Bytes    int64
	State    string // offered, accepted, done, declined, cancelled, failed
	Error    string
}

// JoinRoom adds this computer to the room under name. check verifies its
// pairing code (it should rate-limit and rotate like computer pairing).
func (srv *Server) JoinRoom(name, kind string, check func(string) bool) *Local {
	rm := srv.s.room
	l := &Local{rm: rm, xfers: map[string]*LocalXfer{}}
	rm.mu.Lock()
	m := &member{id: randomID(6), key: randomID(16), kind: kind, paired: map[string]bool{},
		out: make(chan any, 256), online: true, host: true, check: check}
	m.name = rm.uniqueName(m, cleanName(name))
	rm.members[m.id], rm.byKey[m.key] = m, m
	l.m = m
	rm.mu.Unlock()
	go l.loop(m.out)
	rm.broadcastRoster()
	return l
}

// SetName renames this computer in the room.
func (l *Local) SetName(name string) {
	if n := cleanName(name); n != "" {
		l.rm.mu.Lock()
		l.m.name = l.rm.uniqueName(l.m, n)
		l.rm.mu.Unlock()
		l.rm.broadcastRoster()
	}
}

// Peers returns the other members, sorted by the room.
func (l *Local) Peers() []RoomPeer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RoomPeer(nil), l.peers...)
}

// Xfer returns a snapshot of one transfer.
func (l *Local) Xfer(id string) (LocalXfer, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	x, ok := l.xfers[id]
	if !ok {
		return LocalXfer{}, false
	}
	return *x, true
}

// Incoming returns transfers other members are offering this computer.
func (l *Local) Incoming() []LocalXfer {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []LocalXfer
	for _, x := range l.xfers {
		if !x.Out && x.State == "offered" {
			out = append(out, *x)
		}
	}
	return out
}

// loop turns room messages for this member into state the app can read.
func (l *Local) loop(out chan any) {
	for v := range out {
		msg, ok := v.(roomMsg)
		if !ok {
			continue
		}
		l.mu.Lock()
		switch msg.Type {
		case "roster":
			l.peers = l.peers[:0]
			for _, p := range msg.Members {
				l.peers = append(l.peers, RoomPeer{ID: p.ID, Name: p.Name, Kind: p.Kind, Paired: p.Paired, Online: p.Online})
			}
		case "incoming":
			files := make([]RoomFile, len(msg.Files))
			for i, f := range msg.Files {
				files[i] = RoomFile(f)
			}
			l.xfers[msg.Xfer] = &LocalXfer{ID: msg.Xfer, Peer: msg.From, PeerName: msg.FromName, Files: files, Total: msg.Total, State: "offered"}
		case "accepted":
			if x := l.xfers[msg.Xfer]; x != nil {
				x.State = "accepted"
			}
		case "progress":
			if x := l.xfers[msg.Xfer]; x != nil {
				x.Bytes = msg.Bytes
			}
		case "done", "declined", "cancelled", "failed":
			if x := l.xfers[msg.Xfer]; x != nil {
				x.State, x.Error = msg.Type, msg.Error
				if msg.Type == "done" {
					x.Bytes = x.Total
				}
				id := x.ID
				time.AfterFunc(roomKeepDone, func() {
					l.mu.Lock()
					delete(l.xfers, id)
					l.mu.Unlock()
				})
			}
		}
		l.mu.Unlock()
	}
}

// Offer asks member to to accept files, checking code unless they're paired.
// After they accept, open(i) supplies file i; it's read to the end and closed.
func (l *Local) Offer(to, code string, files []RoomFile, open func(i int) (io.ReadCloser, error)) (string, error) {
	in := make([]rfile, len(files))
	for i, f := range files {
		in[i] = rfile(f)
	}
	x, err := l.rm.makeOffer(l.m, to, code, in)
	if err != nil {
		return "", err
	}
	sent := make([]RoomFile, len(x.files))
	for i, f := range x.files {
		sent[i] = RoomFile(f)
	}
	l.mu.Lock()
	l.xfers[x.id] = &LocalXfer{ID: x.id, Out: true, Peer: to, PeerName: l.rm.nameOf(to), Files: sent, Total: x.total, State: "offered"}
	l.mu.Unlock()
	go func() {
		for i := range x.files {
			f, err := open(i)
			if err == nil {
				err = l.rm.feed(x, i, f)
				f.Close()
			}
			if err != nil {
				l.rm.finish(x, "failed", err.Error())
				return
			}
		}
	}()
	return x.id, nil
}

// feed hands file i of x to the relay once the receiver has accepted, and
// waits until it's through.
func (rm *room) feed(x *rxfer, i int, body io.Reader) error {
	select {
	case <-x.accepted:
	case <-x.ctx.Done():
		return context.Cause(x.ctx)
	}
	p := pipe{idx: i, body: body, done: make(chan error, 1)}
	select {
	case x.pipes <- p:
	case <-x.ctx.Done():
		return context.Cause(x.ctx)
	case <-time.After(roomPipeWait):
		return errors.New("the receiver isn't downloading")
	}
	return <-p.done // a local file always reaches EOF, so this can't hang on cancel
}

// Accept takes the offered transfer id and writes its files into dir, as
// <name>.bitflow-partial renamed when complete. Existing files are never
// overwritten: a clash becomes "name (1).ext". It returns when done.
func (l *Local) Accept(id, dir string) error {
	rm := l.rm
	rm.mu.Lock()
	x := rm.xfers[id]
	rm.mu.Unlock()
	if x == nil || !rm.accept(l.m, x) {
		return errors.New("that transfer is no longer waiting")
	}
	l.mu.Lock()
	if lx := l.xfers[id]; lx != nil {
		lx.State = "accepted"
	}
	l.mu.Unlock()
	err := rm.relay(context.Background(), x, func(f rfile) (io.Writer, func(error) error, error) {
		final, err := store.SafeJoin(dir, f.Name)
		if err != nil {
			return nil, nil, err
		}
		final = freeName(final)
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			return nil, nil, err
		}
		partial := final + store.PartialSuffix
		w, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return nil, nil, err
		}
		return w, func(err error) error {
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = os.Rename(partial, freeName(final)) // something may have appeared there meanwhile
			}
			if err != nil {
				os.Remove(partial)
				return fmt.Errorf("%s: %w", f.Name, err)
			}
			return nil
		}, nil
	})
	if err != nil {
		rm.finish(x, "failed", err.Error())
		return err
	}
	rm.finish(x, "done", "")
	return nil
}

// Decline refuses an offer; Cancel stops a transfer either way.
func (l *Local) Decline(id string) { l.end(id, "declined", l.m.name+" declined") }
func (l *Local) Cancel(id string)  { l.end(id, "cancelled", l.m.name+" cancelled") }

func (l *Local) end(id, state, why string) {
	l.rm.mu.Lock()
	x := l.rm.xfers[id]
	l.rm.mu.Unlock()
	if x != nil && (x.from == l.m.id || x.to == l.m.id) {
		l.rm.finish(x, state, why)
	}
}
