package nattraversal

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/getlantern/flashlight/log"
	"github.com/getlantern/go-natty/natty"
	"github.com/getlantern/waddell"
)

const (
	MaxWaddellMessageSize = 4096
)

const (
	MaxMessageSize    = 4096
	NumUDPTestPackets = 10
	Ready             = "Ready"
	Timeout           = 15 * time.Second
)

type Peers map[waddell.PeerId]*Peer

type Peer struct {
	id              waddell.PeerId
	traversals      map[uint32]*natty.Traversal
	traversalsMutex sync.Mutex
}

type PeerConfig struct {
	Id          string
	WaddellAddr string
}

type WaddellConn struct {
	client *waddell.Client
	conn   net.Conn
}

type message []byte

func (msg message) setTraversalId(id uint32) {
	endianness.PutUint32(msg[:4], id)
}

func (msg message) getTraversalId() uint32 {
	return endianness.Uint32(msg[:4])
}

func (msg message) getData() []byte {
	return msg[4:]
}

var (
	endianness   = binary.LittleEndian
	WaddellConns map[string]*WaddellConn
	peers        Peers
	peersMutex   sync.Mutex
	debugOut     io.Writer
	serverReady  = make(chan bool, NumUDPTestPackets)
)

func init() {
	WaddellConns = make(map[string]*WaddellConn)
	peers = make(map[waddell.PeerId]*Peer)
	//debugOut = os.Stderr
}

func idToBytes(id uint32) []byte {
	b := make([]byte, 4)
	endianness.PutUint32(b[:4], id)
	return b
}

func ConnectToWaddell(waddellAddr string) (err error, wc *WaddellConn) {
	conn, err := net.DialTimeout("tcp", waddellAddr, 20*time.Second)
	if err != nil {
		return
	}
	client, err := waddell.Connect(conn)

	if err != nil {
		log.Errorf("Unable to connect to waddell: %s", err)
	} else {
		log.Debugf("Connected to Waddell!!! Id is: %s", client.ID())
		wc = &WaddellConn{
			client: client,
			conn:   conn,
		}
		WaddellConns[waddellAddr] = wc
	}
	return
}

func CheckPeersList(configPeers *[]PeerConfig) {
	for _, peer := range *configPeers {
		peerId, err := waddell.PeerIdFromString(peer.Id)
		if err != nil {
			log.Errorf("Unable to parse PeerID for server %s: %s",
				peer.Id, err)
		}

		if peers[peerId] != nil {
			continue
		}

		if WaddellConns[peer.WaddellAddr] == nil {
			/* new waddell server--open connection to it */
			ConnectToWaddell(peer.WaddellAddr)
		}

		log.Debugf("Sending offer to peer %s", peer.Id)
		sendOffer(peer.WaddellAddr, peerId)
	}
}

func sendMessages(wc *WaddellConn, t *natty.Traversal, peerId waddell.PeerId,
	traversalId uint32) {
	for {
		msgOut, done := t.NextMsgOut()
		if done {
			return
		}
		log.Debugf("Sending %s", msgOut)
		wc.client.SendPieces(peerId, idToBytes(traversalId), []byte(msgOut))
	}
}

func receiveMessages(wc *WaddellConn, t *natty.Traversal,
	traversalId uint32) {
	b := make([]byte, MaxMessageSize+waddell.WADDELL_OVERHEAD)
	for {
		wm, err := wc.client.Receive(b)
		if err != nil {
			log.Fatalf("Unable to read message from waddell: %s", err)
		}
		msg := message(wm.Body)
		if msg.getTraversalId() != traversalId {
			log.Debugf("Got message for unknown traversal %d, skipping", msg.getTraversalId())
			continue
		}
		log.Debugf("Received: %s", msg.getData())
		msgString := string(msg.getData())
		if Ready == msgString {
			// Server's ready!
			serverReady <- true
		} else {
			t.MsgIn(msgString)
		}
	}
}

func sendOffer(waddellAddr string, peerId waddell.PeerId) {
	wc := WaddellConns[waddellAddr]

	traversalId := uint32(rand.Int31())
	log.Debugf("Starting traversal: %d", traversalId)

	t := natty.Offer(debugOut)

	p := &Peer{
		id:         peerId,
		traversals: make(map[uint32]*natty.Traversal),
	}
	p.traversals[traversalId] = t
	peers[peerId] = p

	go sendMessages(wc, t, peerId, traversalId)
	go receiveMessages(wc, t, traversalId)

	ft, err := t.FiveTupleTimeout(Timeout)
	if err != nil {
		log.Fatalf("Unable to offer: %s", err)
	}
	log.Debugf("Got five tuple: %s", ft)
	if <-serverReady {
		writeUDP(ft)
	}
}

func writeUDP(ft *natty.FiveTuple) {
	local, remote, err := ft.UDPAddrs()
	if err != nil {
		log.Fatalf("Unable to resolve UDP addresses: %s", err)
	}
	conn, err := net.DialUDP("udp", local, remote)
	if err != nil {
		log.Fatalf("Unable to dial UDP: %s", err)
	}
	for i := 0; i < NumUDPTestPackets; i++ {
		msg := fmt.Sprintf("Hello from %s to %s", ft.Local, ft.Remote)
		log.Debugf("Sending UDP message: %s", msg)
		_, err := conn.Write([]byte(msg))
		if err != nil {
			log.Fatalf("Offerer unable to write to UDP: %s", err)
		}
		time.Sleep(1 * time.Second)
	}
	conn.Close()
}

func readUDP(wc *waddell.Client, peerId waddell.PeerId, traversalId uint32, ft *natty.FiveTuple) {
	local, _, err := ft.UDPAddrs()
	if err != nil {
		log.Fatalf("Unable to resolve UDP addresses: %s", err)
	}
	conn, err := net.ListenUDP("udp", local)
	if err != nil {
		log.Fatalf("Unable to listen on UDP: %s", err)
	}
	log.Debugf("Listening for UDP packets at: %s", local)
	notifyClientOfServerReady(wc, peerId, traversalId)
	b := make([]byte, 1024)
	for {
		n, addr, err := conn.ReadFrom(b)
		if err != nil {
			log.Fatalf("Unable to read from UDP: %s", err)
		}
		msg := string(b[:n])
		log.Debugf("Got UDP message from %s: '%s'", addr, msg)
	}
}

func notifyClientOfServerReady(wc *waddell.Client, peerId waddell.PeerId, traversalId uint32) {
	wc.SendPieces(peerId, idToBytes(traversalId), []byte(Ready))
}

func ReceiveOffers(waddellAddr string) {
	for {
		wc := WaddellConns[waddellAddr]
		if wc == nil {
			continue
		}
		b := make([]byte, MaxWaddellMessageSize+waddell.WADDELL_OVERHEAD)
		wm, err := wc.client.Receive(b)
		if err != nil {
			log.Errorf("Unable to read message from waddell: %s", err)
			if err != io.EOF || err != io.ErrUnexpectedEOF {
				return
			}
			continue
		}
		msg := []byte(wm.Body)
		log.Debugf("Peer ID is %s", wm.From.String())
		log.Debugf("Received waddell message: %s", msg[4:])
		answer(wc.client, wm)
	}
}

func CloseWaddellConn(waddellAddr string) {
	wc := WaddellConns[waddellAddr]
	if wc != nil {
		log.Debugf("Closing WaddellConn")
		wc.conn.Close()
		log.Debugf("Closed WaddellConn")
		delete(WaddellConns, waddellAddr)
		wc = nil
	}
}

func answer(wc *waddell.Client, wm *waddell.Message) {
	peersMutex.Lock()
	defer peersMutex.Unlock()
	p := peers[wm.From]
	if p == nil {
		p = &Peer{
			id:         wm.From,
			traversals: make(map[uint32]*natty.Traversal),
		}
		peers[wm.From] = p
	}
	p.answer(wc, wm)
}

func (p *Peer) answer(wc *waddell.Client, wm *waddell.Message) {
	p.traversalsMutex.Lock()
	defer p.traversalsMutex.Unlock()
	msg := message(wm.Body)
	traversalId := msg.getTraversalId()
	t := p.traversals[traversalId]
	if t == nil {
		log.Debugf("Answering traversal: %d", traversalId)
		// Set up a new Natty traversal
		t = natty.Answer(debugOut)
		go func() {
			// Send
			for {
				msgOut, done := t.NextMsgOut()
				if done {
					return
				}
				log.Debugf("Sending %s", msgOut)
				wc.SendPieces(p.id, idToBytes(traversalId), []byte(msgOut))
			}
		}()

		go func() {
			// Receive
			defer func() {
				p.traversalsMutex.Lock()
				defer p.traversalsMutex.Unlock()
				delete(p.traversals, traversalId)
			}()

			ft, err := t.FiveTupleTimeout(Timeout)
			if err != nil {
				log.Debugf("Unable to answer traversal %d: %s", traversalId, err)
				return
			}

			log.Debugf("Got five tuple: %s", ft)
			go readUDP(wc, p.id, traversalId, ft)
		}()
		p.traversals[traversalId] = t
	}
	log.Debugf("Received for traversal %d: %s", traversalId, msg.getData())
	t.MsgIn(string(msg.getData()))
}
