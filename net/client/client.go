package cherryClient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	cherryCompress "github.com/cherry-game/cherry/extend/compress"
	cherryFacade "github.com/cherry-game/cherry/facade"
	cherryLogger "github.com/cherry-game/cherry/logger"
	cherryMessage "github.com/cherry-game/cherry/net/message"
	cherryPacket "github.com/cherry-game/cherry/net/packet"
)

type IClient interface {
	ConnectTo(addr string) error
	ConnectToTLS(addr string, skipVerify bool) error
	Disconnect()
	SendNotify(route string, data []byte) error
	SendRequest(route string, data []byte) (uint, error)
	ConnectedStatus() bool
	MsgChannel() chan *cherryMessage.Message
}

var (
	handshakeBuffer = `
{
	"sys": {
		"platform": "mac",
		"libVersion": "0.3.5-release",
		"clientBuildNumber":"20",
		"clientVersion":"2.1"
	},
	"user": {
		"age": 30
	}
}
`
)

// HandshakeSys struct
type HandshakeSys struct {
	Dict       map[string]uint16 `json:"dict"`
	Heartbeat  int               `json:"heartbeat"`
	Serializer string            `json:"serializer"`
}

// HandshakeData struct
type HandshakeData struct {
	Code int          `json:"code"`
	Sys  HandshakeSys `json:"sys"`
}

type pendingRequest struct {
	msg    *cherryMessage.Message
	sentAt time.Time
}

// Client struct
type Client struct {
	conn            INetConn
	Connected       bool
	packetCodec     cherryFacade.IPacketCodec
	packetChan      chan cherryFacade.IPacket
	IncomingMsgChan chan *cherryMessage.Message
	pendingChan     chan bool
	pendingRequests map[uint]*pendingRequest
	pendingReqMutex sync.Mutex
	requestTimeout  time.Duration
	closeChan       chan struct{}
	nextID          uint32
}

// MsgChannel return the incoming message channel
func (c *Client) MsgChannel() chan *cherryMessage.Message {
	return c.IncomingMsgChan
}

// ConnectedStatus return the connection status
func (c *Client) ConnectedStatus() bool {
	return c.Connected
}

// New returns a new client
func New(conn INetConn, requestTimeout ...time.Duration) *Client {
	reqTimeout := 5 * time.Second
	if len(requestTimeout) > 0 {
		reqTimeout = requestTimeout[0]
	}

	return &Client{
		conn:            conn,
		Connected:       false,
		packetCodec:     cherryPacket.NewPomeloCodec(),
		packetChan:      make(chan cherryFacade.IPacket, 10),
		pendingRequests: make(map[uint]*pendingRequest),
		requestTimeout:  reqTimeout,
		// 30 here is the limit of inflight messages
		// TODO this should probably be configurable
		pendingChan: make(chan bool, 30),
		//messageEncoder: message.NewMessagesEncoder(true),
	}
}

func (c *Client) sendHandshakeRequest() error {
	p, err := c.packetCodec.PacketEncode(cherryPacket.Handshake, []byte(handshakeBuffer))
	if err != nil {
		return err
	}
	_, err = c.conn.Write(p)
	return err
}

func (c *Client) handleHandshakeResponse() error {
	buf := bytes.NewBuffer(nil)
	packets, err := c.readPackets(buf)
	if err != nil {
		return err
	}

	handshakePacket := packets[0]
	if handshakePacket.Type() != cherryPacket.Handshake {
		return fmt.Errorf("got first packet from server that is not a handshake, aborting")
	}

	handshake := &HandshakeData{}
	if cherryCompress.IsCompressed(handshakePacket.Data()) {
		data, err := cherryCompress.InflateData(handshakePacket.Data())
		if err != nil {
			return err
		}
		handshakePacket.SetData(data)
	}

	err = json.Unmarshal(handshakePacket.Data(), handshake)
	if err != nil {
		return err
	}

	cherryLogger.Debugf("got handshake from sv, data: %v", handshake)

	if handshake.Sys.Dict != nil {
		cherryMessage.SetDictionary(handshake.Sys.Dict)
	}

	p, err := c.packetCodec.PacketEncode(cherryPacket.HandshakeAck, []byte{})
	if err != nil {
		return err
	}
	_, err = c.conn.Write(p)
	if err != nil {
		return err
	}

	c.Connected = true

	if handshake.Sys.Heartbeat < 1 {
		handshake.Sys.Heartbeat = 3
	}

	go c.sendHeartbeats(handshake.Sys.Heartbeat)
	go c.handleServerMessages()
	go c.handlePackets()
	go c.pendingRequestsReaper()

	return nil
}

// pendingRequestsReaper delete timedout requests
func (c *Client) pendingRequestsReaper() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			toDelete := make([]*pendingRequest, 0)
			c.pendingReqMutex.Lock()
			for _, v := range c.pendingRequests {
				if time.Now().Sub(v.sentAt) > c.requestTimeout {
					toDelete = append(toDelete, v)
				}
			}
			for _, pendingReq := range toDelete {
				err := errors.New("request timeout")
				errMarshalled, _ := json.Marshal(err)
				// send a timeout to incoming msg chan
				m := &cherryMessage.Message{
					Type:  cherryMessage.Response,
					ID:    pendingReq.msg.ID,
					Route: pendingReq.msg.Route,
					Data:  errMarshalled,
				}
				delete(c.pendingRequests, pendingReq.msg.ID)
				<-c.pendingChan
				c.IncomingMsgChan <- m
			}
			c.pendingReqMutex.Unlock()
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) handlePackets() {
	for {
		select {
		case p := <-c.packetChan:
			switch p.Type() {
			case cherryPacket.Data:
				//handle data
				cherryLogger.Debugf("got data: %s", string(p.Data()))
				m, err := cherryMessage.Decode(p.Data())
				if err != nil {
					cherryLogger.Errorf("error decoding msg from sv: %s", string(m.Data))
				}
				if m.Type == cherryMessage.Response {
					c.pendingReqMutex.Lock()
					if _, ok := c.pendingRequests[m.ID]; ok {
						delete(c.pendingRequests, m.ID)
						<-c.pendingChan
					} else {
						c.pendingReqMutex.Unlock()
						continue // do not process msg for already timedout request
					}
					c.pendingReqMutex.Unlock()
				}
				c.IncomingMsgChan <- m
			case cherryPacket.Kick:
				cherryLogger.Warn("got kick packet from the server! disconnecting...")
				c.Disconnect()
			case cherryPacket.Heartbeat:
				{
					cherryLogger.Info("response heartbeat")
				}
			}
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) readPackets(buf *bytes.Buffer) ([]cherryFacade.IPacket, error) {
	// listen for sv messages
	// data := make([]byte, 1024)
	// n := len(data)
	// var err error

	// for n == len(data) {
	// 	n, err = c.conn.Read(data)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	buf.Write(data[:n])
	// }

	data, err := c.conn.Read()
	if err != nil {
		return nil, err
	}
	buf.Write(data)

	packets, err := c.packetCodec.PacketDecode(buf.Bytes())
	if err != nil {
		cherryLogger.Errorf("error decoding packet from server: %s", err.Error())
	}
	totalProcessed := 0
	for _, p := range packets {
		totalProcessed += cherryPacket.HeadLength + p.Len()
	}
	buf.Next(totalProcessed)

	return packets, nil
}

func (c *Client) handleServerMessages() {
	buf := bytes.NewBuffer(nil)
	defer c.Disconnect()
	for c.Connected {
		packets, err := c.readPackets(buf)
		if err != nil && c.Connected {
			cherryLogger.Error(err)
			break
		}

		for _, p := range packets {
			c.packetChan <- p
		}
	}
}

func (c *Client) sendHeartbeats(interval int) {
	t := time.NewTicker(time.Duration(interval) * time.Second)
	defer func() {
		t.Stop()
		c.Disconnect()
	}()
	for {
		select {
		case <-t.C:
			p, _ := c.packetCodec.PacketEncode(cherryPacket.Heartbeat, []byte{})
			_, err := c.conn.Write(p)
			if err != nil {
				cherryLogger.Errorf("error sending heartbeat to server: %s", err.Error())
				return
			}
		case <-c.closeChan:
			return
		}
	}
}

// Disconnect disconnects the client
func (c *Client) Disconnect() {
	for c.Connected {
		c.Connected = false
		close(c.closeChan)
		err := c.conn.Close()
		if err != nil {
			cherryLogger.Error(err)
		}
	}
}

// ConnectToTLS connects to the server at addr using TLS, for now the only supported protocol is tcp
// this methods blocks as it also handles the messages from the server
func (c *Client) ConnectToTLS(addr string, skipVerify bool) error {
	// conn, err := tls.Dial("tcp", addr, &tls.Config{
	// 	InsecureSkipVerify: skipVerify,
	// })
	err := c.conn.ConnectToTLS(skipVerify)
	if err != nil {
		return err
	}
	//c.conn = conn
	c.IncomingMsgChan = make(chan *cherryMessage.Message, 10)

	if err = c.handleHandshake(); err != nil {
		return err
	}

	c.closeChan = make(chan struct{})
	return nil
}

// ConnectTo connects to the server at addr, for now the only supported protocol is tcp
// this methods blocks as it also handles the messages from the server
func (c *Client) ConnectTo() error {
	// conn, err := net.Dial("tcp", addr)
	err := c.conn.ConnectTO()
	if err != nil {
		return err
	}
	//c.conn = conn
	c.IncomingMsgChan = make(chan *cherryMessage.Message, 10)

	if err = c.handleHandshake(); err != nil {
		return err
	}

	c.closeChan = make(chan struct{})

	return nil
}

func (c *Client) handleHandshake() error {
	if err := c.sendHandshakeRequest(); err != nil {
		return err
	}

	if err := c.handleHandshakeResponse(); err != nil {
		return err
	}
	return nil
}

// SendRequest sends a request to the server
func (c *Client) SendRequest(route string, data []byte) (uint, error) {
	return c.sendMsg(cherryMessage.Request, route, data)
}

// SendNotify sends a notify to the server
func (c *Client) SendNotify(route string, data []byte) error {
	_, err := c.sendMsg(cherryMessage.Notify, route, data)
	return err
}

func (c *Client) buildPacket(msg cherryMessage.Message) ([]byte, error) {
	encMsg, err := cherryMessage.Encode(&msg)
	if err != nil {
		return nil, err
	}

	p, err := c.packetCodec.PacketEncode(cherryPacket.Data, encMsg)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// sendMsg sends the request to the server
func (c *Client) sendMsg(msgType cherryMessage.Type, route string, data []byte) (uint, error) {
	// TODO mount msg and encode
	m := cherryMessage.Message{
		Type:  msgType,
		ID:    uint(atomic.AddUint32(&c.nextID, 1)),
		Route: route,
		Data:  data,
	}
	p, err := c.buildPacket(m)
	if msgType == cherryMessage.Request {
		c.pendingChan <- true
		c.pendingReqMutex.Lock()
		if _, ok := c.pendingRequests[m.ID]; !ok {
			c.pendingRequests[m.ID] = &pendingRequest{
				msg:    &m,
				sentAt: time.Now(),
			}
		}
		c.pendingReqMutex.Unlock()
	}

	if err != nil {
		return m.ID, err
	}
	_, err = c.conn.Write(p)
	return m.ID, err
}
