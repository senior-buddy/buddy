package carrot

import (
	"bytes"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"math"
	"net/http"
	"sync"
	"time"
)

const (
	writeWaitSeconds = 10 * 10
	pongWaitSeconds  = 10 * 60

	// time allowed to write a message to the websocket
	writeWait = writeWaitSeconds * time.Second

	// time allowed to read the next pong message from the websocket
	pongWait = pongWaitSeconds * time.Second

	// send pings to the websocket with this period, must be less than pongWait
	pingPeriod = (pongWait * 9) / 10

	// maximum message size allowed from the websocket
	maxMessageSize = 8192

	// toggle to require a client secret token on WS upgrade request
	clientSecretRequired = false

	// size of client send channel
	sendMsgBufferSize = 1024

	// size of sendToken channel
	sendTokenBufferSize = 1

	sendMsgBufferWarningTrigger = 0.9
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type Client struct {
	session *Session
	server  *Server
	// acts as a signal for when to start the go routines
	start chan struct{}
	open  bool

	conn *websocket.Conn

	//buffered channel of outbound messages
	send chan []byte

	//buffered channel of outbound tokens
	sendToken chan SessionToken

	openMutex *sync.RWMutex

	logger *log.Entry
}

func (c *Client) Open() bool {
	c.openMutex.RLock()
	status := c.open
	c.openMutex.RUnlock()
	return status
}

func (c *Client) softOpen() {
	c.openMutex.Lock()
	c.open = true
	c.openMutex.Unlock()
}

func (c *Client) softClose() {
	c.openMutex.Lock()
	c.open = false
	c.openMutex.Unlock()
}

func (c *Client) Expired() bool {
	return !c.Open() && c.session.sessionDurationExpired()
}

func (c *Client) Full() bool {
	// check for buffer full
	if len(c.send) == sendMsgBufferSize {
		return true
	}

	return false
}

func (c *Client) Valid() bool {
	// TODO: Specify critirea for what is a "valid" connection aside from existing
	return c != nil
}

func (c *Client) logBufferRedZone() {
	// check for buffer warning
	if len(c.send) > int(math.Floor(sendMsgBufferSize*sendMsgBufferWarningTrigger)) {
		c.logger.WithFields(log.Fields{
			"open?":   c.Open(),
			"size":    len(c.send),
			"channel": "send"}).Error("input channel is 90% full!")
	}
}

func (c *Client) logBufferFull() {
	if len(c.send) == sendMsgBufferSize {
		c.logger.WithFields(log.Fields{
			"open?":   c.Open(),
			"size":    len(c.send),
			"channel": "send"}).Error("input channel is full!")
	}
}

//readPump pumps messages from the websocket to the server
func (c *Client) readPump() {
	defer func() {
		c.server.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.Errorf("error: %v", err)
			}
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))

		req := NewRequest(c.session, message)
		c.logger.WithField("session_token", c.session.Token).Debug("request being sent to middleware")
		c.server.Middleware.In <- req
		//c.server.broadcast <- message
	}
}

//writePump pumps messages from the hub to the websocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// TODO: add session token to here once client list is updated
				c.logger.Error("a connection has closed\n")
				//the server closed the channel
				//c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			//add queued messages to the current websocket message
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write(newline)
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case token, ok := <-c.sendToken:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				//the server closed the channel
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write([]byte(token))

			//add queued messages to the current websocket message
			n := len(c.sendToken)
			for i := 0; i < n; i++ {
				w.Write(newline)
				w.Write([]byte(<-c.sendToken))
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

// validate that the client should be allowed to connect
func validClientSecret(clientSecret string) bool {
	if clientSecret != serverSecret {
		log.WithField("attempted_secret", clientSecret).Error("client and server secrets do not match :(")
		return false
	}

	return true
}

func serveWs(server *Server, w http.ResponseWriter, r *http.Request) {
	sessionToken, clientSecret, _ := r.BasicAuth()
	//log.Printf("Session Token: %v | Client Secret: %v", SessionToken, clientSecret)

	if clientSecretRequired && !validClientSecret(clientSecret) {
		http.Error(w, "Not Authorized!", 403)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := &Client{
		session:   nil,
		server:    server,
		conn:      conn,
		send:      make(chan []byte, sendMsgBufferSize),
		sendToken: make(chan SessionToken, sendTokenBufferSize),
		start:     make(chan struct{}),
		open:      false,
		openMutex: &sync.RWMutex{},
		logger:    log.WithField("module", "client"),
	}

	client.sendToken <- SessionToken(sessionToken)
	client.server.register <- client

	func() {
		// TODO: log session token used here
		log.Debug("a new client has joined")
		<-client.start
		go client.writePump()
		go client.readPump()
	}()
}
