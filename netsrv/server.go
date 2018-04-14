package netsrv

import (
	"log"
	"net"
	"sync"

	"github.com/UniversityRadioYork/baps3d/bifrost"
	"github.com/UniversityRadioYork/baps3d/comm"
)

// Server holds the internal state of a baps3d TCP server.
type Server struct {
	// l is the Server's logger.
	l *log.Logger

	// host is the Server's host:port string.
	host string

	// rootClient is a controller Client the Server can clone for
	// use by incoming connections.
	rootClient *comm.Client

	// rootBifrost is a Bifrost parser the Server can use for
	// incoming connections.
	rootBifrost comm.BifrostParser

	// clients is a map containing all connected clients.
	clients map[client]struct{}

	// accConn is a channel used by the acceptor goroutine to send new
	// connections to the main goroutine.
	accConn chan net.Conn

	// accErr is a channel used by the acceptor goroutine to send errors
	// to the main goroutine.
	// Errors landing from accErr are considered fatal.
	accErr chan error

	// clientHangUp is a channel used by client goroutines to send
	// disconnections to the main goroutine.
	// It sends a pointer to the client to disconnect.
	clientHangUp chan *client

	// clientErr is a channel used by client goroutines to send
	// errors to the main goroutine.
	// The client will send a hangup request if the error is fatal.
	clientErr chan error

	// done is a channel closed when the main loop terminates.
	// This is used to signal all goroutines to close, if they haven't
	// already.
	done chan struct{}

	// wg is a WaitGroup that tracks all inner server goroutines.
	// The server main loop won't terminate until the WaitGroup hits zero.
	wg sync.WaitGroup
}

// client holds the server-side state of a baps3d TCP client.
type client struct {
	// l holds the logger for this client.
	l *log.Logger

	// conn holds the client socket.
	conn net.Conn

	// buf holds the client buffer.
	buf [4096]byte

	// conClient is the client's Client for the Controller for this
	// server.
	conClient *comm.Client

	// conBifrost is the Bifrost adapter for conClient.
	conBifrost *comm.BifrostClient

	// srvHangup is the channel to send the client to when it hangs up.
	//
}

// Close closes the given client.
func (c *client) Close() error {
	// TODO(@MattWindsor91): disconnect client and bifrost
	return c.conn.Close()
}

// New creates a new network server for a baps3d instance.
func New(l *log.Logger, host string, rc *comm.Client, rb comm.BifrostParser) *Server {
	return &Server{
		l:            l,
		host:         host,
		rootClient:   rc,
		rootBifrost:  rb,
		accConn:      make(chan net.Conn),
		accErr:       make(chan error),
		clientHangUp: make(chan *client),
		clientErr:    make(chan error),
		done:         make(chan struct{}),
		clients:      make(map[client]struct{}),
	}
}

func (s *Server) shutdownController() {
	s.l.Println("shutting down")
	s.rootClient.Shutdown()
}

// newClient sets up the server s to handle incoming connection c.
func (s *Server) newClient(c net.Conn) error {
	s.l.Println("new connection:", c.RemoteAddr().String())

	conClient, err := s.rootClient.Copy()
	if err != nil {
		_ = c.Close()
		return err
	}
	conBifrost, conBifrostClient := comm.NewBifrost(conClient, s.rootBifrost)
	cli := client{
		conn:       c,
		conClient:  conClient,
		conBifrost: conBifrostClient,
		l:          s.l,
	}

	s.clients[cli] = struct{}{}

	s.wg.Add(3)
	go func() {
		cli.RunTx()
		// Only hang up if the server is still around.
		// Otherwise, we'll just hang here waiting for the server to answer,
		// while the server hangs up the client anyway.
		select {
		case s.clientHangUp <- &cli:
		case <-s.done:
		}
		s.wg.Done()
	}()
	go func() {
		cli.RunRx()
		s.wg.Done()
	}()
	go func() {
		conBifrost.Run()
		s.wg.Done()
	}()

	return nil
}

// RunRx runs the client's message receiver loop.
// This writes messages to the socket.
func (c *client) RunRx() {
	// We don't have to check c.bclient.Done here:
	// client always drops both Rx and Done when shutting down.
	for m := range c.conBifrost.Rx {
		mbytes, err := m.Pack()
		if err != nil {
			c.outputError(err)
			continue
		}

		if _, err := c.conn.Write(mbytes); err != nil {
			c.outputError(err)
			break
		}
	}
}

// outputError logs a connection error for client c.
func (c *client) outputError(e error) {
	c.l.Println("connection error:", e.Error())
}

// RunTx runs the client's message transmitter loop.
// This reads from stdin.
func (c *client) RunTx() {
	r := bifrost.NewReaderTokeniser(c.conn)

	for {
		line, terr := r.ReadLine()
		if terr != nil {
			c.outputError(terr)
			break
		}

		msg, merr := bifrost.LineToMessage(line)
		if merr != nil {
			c.outputError(merr)
			break
		}

		if !c.conBifrost.Send(*msg) {
			c.l.Println("client died while sending message")
			break
		}
	}
}

// hangUpAllClients gracefully closes all connected clients on s.
func (s *Server) hangUpAllClients() {
	for c := range s.clients {
		s.hangUpClient(&c)
	}
}

// hangUpClient closes the client pointed to by c.
func (s *Server) hangUpClient(c *client) {
	cname := c.conn.RemoteAddr().String()
	s.l.Println("hanging up:", cname)
	if err := c.Close(); err != nil {
		s.l.Printf("couldn't gracefully close %s: %s\n", cname, err.Error())
	}
	delete(s.clients, *c)
}

// Run prepares and runs the net server main loop.
func (s *Server) Run() {
	defer s.wg.Wait()
	defer s.shutdownController()

	ln, err := net.Listen("tcp", s.host)
	if err != nil {
		s.l.Println("couldn't open server:", err)
		return
	}

	s.l.Println("now listening on", s.host)
	s.wg.Add(1)
	go func() {
		s.acceptClients(ln)
		s.wg.Done()
	}()

	s.mainLoop()

	close(s.done)
	s.hangUpAllClients()
	if err := ln.Close(); err != nil {
		s.l.Println("error closing listener:", err)
	}
	s.l.Println("closed listener")
}

// mainLoop is the server's main connection handling loop.
func (s *Server) mainLoop() {
	for {
		select {
		case err := <-s.accErr:
			s.l.Println("error accepting connections:", err)
			return
		case conn := <-s.accConn:
			cname := conn.RemoteAddr().String()
			if err := s.newClient(conn); err != nil {
				s.l.Printf("error registering connection %s: %s\n", cname, err.Error())
			}
		case c := <-s.clientHangUp:
			s.hangUpClient(c)
		case <-s.rootClient.Rx:
			// Drain any messages sent to the root client.
		case <-s.rootClient.Done:
			s.l.Println("received controller shutdown")
			return
		}
	}
}

// acceptClients keeps spinning, accepting clients on ln and sending them to
// connCh, until ln closes.
// It then sends the error on errCh and closes both channels.
func (s *Server) acceptClients(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Only send the error if the main loop is listening
			select {
			case s.accErr <- err:
			case <-s.done:
			}
			close(s.accErr)
			close(s.accConn)
			return
		}

		// Only forward connections if the main loop actually wants them
		select {
		case s.accConn <- conn:
		case <-s.done:
			// TODO(@MattWindsor91): necessary?
			_ = conn.Close()
		}
	}
}
