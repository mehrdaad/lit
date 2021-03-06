package lndc

import (
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/mit-dci/lit/crypto/koblitz"
)

// defaultHandshakes is the maximum number of handshakes that can be done in
// parallel.
const defaultHandshakes = 1000

// Listener is an implementation of a net.Conn which executes an authenticated
// key exchange and message encryption protocol dubbed "Machine" after
// initial connection acceptance. See the Machine struct for additional
// details w.r.t the handshake and encryption scheme used within the
// connection.
type Listener struct {
	localStatic *koblitz.PrivateKey

	tcp *net.TCPListener

	handshakeSema chan struct{}
	conns         chan maybeConn
	quit          chan struct{}
}

// A compile-time assertion to ensure that Conn meets the net.Listener interface.
var _ net.Listener = (*Listener)(nil)

// NewListener returns a new net.Listener which enforces the lndc scheme
// during both initial connection establishment and data transfer.
func NewListener(localStatic *koblitz.PrivateKey, port int) (*Listener,
	error) {
	// since this is a listener, it is sufficient that we just pass the
	// port and then add the later stuff here
	str := ":" + strconv.Itoa(port) // colonize!
	addr, err := net.ResolveTCPAddr("tcp", str)
	if err != nil {
		return nil, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return nil, err
	}

	lndcListener := &Listener{
		localStatic:   localStatic,
		tcp:           l,
		handshakeSema: make(chan struct{}, defaultHandshakes),
		conns:         make(chan maybeConn),
		quit:          make(chan struct{}),
	}

	for i := 0; i < defaultHandshakes; i++ {
		lndcListener.handshakeSema <- struct{}{}
	}

	go lndcListener.listen()

	return lndcListener, nil
}

// listen accepts connection from the underlying tcp conn, then performs
// the brontinde handshake procedure asynchronously. A maximum of
// defaultHandshakes will be active at any given time.
//
// NOTE: This method must be run as a goroutine.
func (l *Listener) listen() {
	for {
		select {
		case <-l.handshakeSema:
		case <-l.quit:
			return
		}

		conn, err := l.tcp.Accept()
		if err != nil {
			l.rejectConn(err)
			l.handshakeSema <- struct{}{}
			continue
		}

		go l.doHandshake(conn)
	}
}

// doHandshake asynchronously performs the lndc handshake, so that it does
// not block the main accept loop. This prevents peers that delay writing to the
// connection from block other connection attempts.
func (l *Listener) doHandshake(conn net.Conn) {
	defer func() { l.handshakeSema <- struct{}{} }()

	select {
	case <-l.quit:
		return
	default:
	}

	lndcConn := &Conn{
		conn:  conn,
		noise: NewNoiseMachine(false, l.localStatic),
	}

	// We'll ensure that we get ActOne from the remote peer in a timely
	// manner. If they don't respond within 1s, then we'll kill the
	// connection.
	conn.SetReadDeadline(time.Now().Add(handshakeReadTimeout))

	// Attempt to carry out the first act of the handshake protocol. If the
	// connecting node doesn't know our long-term static public key, then
	// this portion will fail with a non-nil error.
	var actOne [ActOneSize]byte
	if _, err := io.ReadFull(conn, actOne[:]); err != nil {
		lndcConn.conn.Close()
		l.rejectConn(err)
		return
	}
	if err := lndcConn.noise.RecvActOne(actOne); err != nil {
		lndcConn.conn.Close()
		l.rejectConn(err)
		return
	}
	// Next, progress the handshake processes by sending over our ephemeral
	// key for the session along with an authenticating tag.
	actTwo, err := lndcConn.noise.GenActTwo()
	if err != nil {
		lndcConn.conn.Close()
		l.rejectConn(err)
		return
	}
	if _, err := conn.Write(actTwo[:]); err != nil {
		lndcConn.conn.Close()
		l.rejectConn(err)
		return
	}

	select {
	case <-l.quit:
		return
	default:
	}

	// We'll ensure that we get ActTwo from the remote peer in a timely
	// manner. If they don't respond within 1 second, then we'll kill the
	// connection.
	conn.SetReadDeadline(time.Now().Add(handshakeReadTimeout))

	// Finally, finish the handshake processes by reading and decrypting
	// the connection peer's static public key. If this succeeds then both
	// sides have mutually authenticated each other.
	var actThree [ActThreeSize]byte
	if _, err := io.ReadFull(conn, actThree[:]); err != nil {
		lndcConn.conn.Close()
		l.rejectConn(err)
		return
	}
	if err := lndcConn.noise.RecvActThree(actThree); err != nil {
		lndcConn.conn.Close()
		l.rejectConn(err)
		return
	}

	// We'll reset the deadline as it's no longer critical beyond the
	// initial handshake.
	conn.SetReadDeadline(time.Time{})

	l.acceptConn(lndcConn)
}

// maybeConn holds either a lndc connection or an error returned from the
// handshake.
type maybeConn struct {
	conn *Conn
	err  error
}

// acceptConn returns a connection that successfully performed a handshake.
func (l *Listener) acceptConn(conn *Conn) {
	select {
	case l.conns <- maybeConn{conn: conn}:
	case <-l.quit:
	}
}

// rejectConn returns any errors encountered during connection or handshake.
func (l *Listener) rejectConn(err error) {
	select {
	case l.conns <- maybeConn{err: err}:
	case <-l.quit:
	}
}

// Accept waits for and returns the next connection to the listener. All
// incoming connections are authenticated via the three act lndc
// key-exchange scheme. This function will fail with a non-nil error in the
// case that either the handshake breaks down, or the remote peer doesn't know
// our static public key.
//
// Part of the net.Listener interface.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case result := <-l.conns:
		return result.conn, result.err
	case <-l.quit:
		return nil, errors.New("lndc connection closed")
	}
}

// Close closes the listener.  Any blocked Accept operations will be unblocked
// and return errors.
//
// Part of the net.Listener interface.
func (l *Listener) Close() error {
	select {
	case <-l.quit:
	default:
		close(l.quit)
	}

	return l.tcp.Close()
}

// Addr returns the listener's network address.
//
// Part of the net.Listener interface.
func (l *Listener) Addr() net.Addr {
	return l.tcp.Addr()
}
