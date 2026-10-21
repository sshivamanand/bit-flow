package ui

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sshivamanand/bit-flow/internal/awake"
	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/web"
)

// The app is a member of the browser room, so phones and browsers on the
// Wi-Fi show up under "nearby" next to bit-flow computers. Sending to one of
// them goes through the room (the slow lane, plain HTTP); computers running
// bit-flow always get the direct, encrypted path.

// roomPrefix marks a room member in a send target ("room:<id>").
const roomPrefix = "room:"

// RoomPeer is a phone or browser in the room, as the UI lists it.
type RoomPeer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Paired bool   `json:"paired"`
}

func roomKind(goos string) string {
	switch goos {
	case "darwin":
		return "Mac"
	case "windows":
		return "Windows PC"
	}
	return "Linux"
}

// roomPeers lists the room's online members. Caller holds a.mu.
func (a *App) roomPeers() []RoomPeer {
	if a.room == nil {
		return nil
	}
	var out []RoomPeer
	for _, p := range a.room.Peers() {
		if p.Online {
			out = append(out, RoomPeer{ID: p.ID, Name: p.Name, Kind: p.Kind, Paired: p.Paired})
		}
	}
	return out
}

// roomOffers turns room transfers waiting for this computer into offers the
// UI asks about, like any other incoming transfer. Caller holds a.mu.
func (a *App) roomOffers() []*offer {
	if a.room == nil {
		return nil
	}
	var out []*offer
	for _, x := range a.room.Incoming() {
		top := roomTop(x.Files)
		out = append(out, &offer{ID: x.ID, From: x.PeerName, Name: summarize(top), Top: top, Files: len(x.Files),
			Total: x.Total, Dest: a.cfg.ReceiveDir(), Via: "room"})
	}
	return out
}

func roomTop(files []web.RoomFile) []string {
	seen := map[string]bool{}
	var top []string
	for _, f := range files {
		t, _, _ := strings.Cut(f.Name, "/")
		if !seen[t] {
			seen[t] = true
			top = append(top, t)
		}
	}
	return top
}

// decideRoom answers a room offer. It reports false if id isn't one.
func (a *App) decideRoom(id string, accept bool, dest string) bool {
	if a.room == nil {
		return false
	}
	lx, ok := a.room.Xfer(id)
	if !ok || lx.Out || lx.State != "offered" {
		return false
	}
	if !accept {
		a.room.Decline(id)
		return true
	}
	if dest == "" {
		dest = a.cfg.ReceiveDir()
	}
	top := roomTop(lx.Files)
	x := &xfer{ID: newID(), Dir: "in", Peer: lx.PeerName, Name: summarize(top), Files: len(lx.Files), Total: lx.Total,
		State: "running", Dest: dest, Started: time.Now(), lastT: time.Now(), Link: "room (browser)",
		cancel: func() { a.room.Cancel(id) }, release: awake.Hold()}
	if len(top) == 1 {
		x.Dest = filepath.Join(dest, filepath.FromSlash(top[0]))
	}
	x.Items, x.More = roomItems(lx.Files)
	x.progress = func() (int64, bool) {
		cur, _ := a.room.Xfer(id)
		return cur.Bytes, true
	}
	a.mu.Lock()
	a.transfers = append([]*xfer{x}, a.transfers...)
	a.mu.Unlock()
	go func() {
		err := a.room.Accept(id, dest)
		a.finishRoom(x, id, err)
	}()
	return true
}

func roomItems(files []web.RoomFile) ([]item, int) {
	var items []item
	for i, f := range files {
		if i == itemsLimit {
			return items, len(files) - i
		}
		items = append(items, item{Name: f.Name, Size: f.Size})
	}
	return items, 0
}

// sendRoom offers paths to a phone or browser in the room.
func (a *App) sendRoom(id, peerName string, paths []string, code string) (string, error) {
	if a.room == nil {
		return "", errors.New("the room isn't running")
	}
	m, err := planner.Build(paths)
	if err != nil {
		return "", err
	}
	var files []web.RoomFile
	var srcs []string
	for i, f := range m.Files {
		if f.Dir || f.Link != "" {
			continue // browsers get plain files; folders come from the paths
		}
		files = append(files, web.RoomFile{Name: f.Path, Size: f.Size})
		srcs = append(srcs, m.Sources[i])
	}
	if len(files) == 0 {
		return "", errors.New("there are no files in what you picked")
	}
	rid, err := a.room.Offer(id, secure.NormalizeCode(code), files, func(i int) (io.ReadCloser, error) { return os.Open(srcs[i]) })
	if err != nil {
		return "", err
	}
	top, _ := topLevel(m.Files)
	x := &xfer{ID: newID(), Dir: "out", Peer: peerName, Name: summarize(top), Files: len(files), Total: m.Total,
		State: "waiting", Started: time.Now(), Link: "room (browser)", cancel: func() { a.room.Cancel(rid) }}
	x.Items, x.More = roomItems(files)
	x.progress = func() (int64, bool) {
		cur, _ := a.room.Xfer(rid)
		return cur.Bytes, cur.State != "offered"
	}
	a.mu.Lock()
	a.transfers = append([]*xfer{x}, a.transfers...)
	a.mu.Unlock()
	go func() {
		release := awake.Hold()
		defer release()
		for {
			cur, ok := a.room.Xfer(rid)
			if !ok || (cur.State != "offered" && cur.State != "accepted") {
				var err error
				if ok && cur.State != "done" {
					err = errors.New(cur.Error)
				}
				a.finishRoom(x, rid, err)
				return
			}
			select {
			case <-a.ctx.Done():
				a.room.Cancel(rid)
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	return x.ID, nil
}

// finishRoom records how a room transfer ended.
func (a *App) finishRoom(x *xfer, id string, err error) {
	cur, _ := a.room.Xfer(id)
	if cur.State == "cancelled" {
		err = errors.New(cur.Error) // "… cancelled"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if x.release != nil {
		x.release()
		x.release = nil
	}
	res := transfer.Result{Bytes: cur.Bytes, Files: x.Files}
	if err == nil {
		res.Bytes = x.Total
	}
	if cur.State == "declined" { // a plain no, not a failure
		x.Finished, x.Rate, x.cancel, x.State = time.Now(), 0, nil, "declined"
		logHistory(x, res)
	} else {
		finish(x, res, err)
	}
	a.saveHistoryLocked()
}
