package websocket

import (
	"bytes"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSClient websocket 客户端
type WSClient struct {
	ws                       *websocket.Conn
	writerMu                 sync.Mutex
	evtMessagePrefix         []byte
	messageType              int
	readTimeout              time.Duration
	WriteTimeout             time.Duration
	PingPeriod               time.Duration
	messageSerializer        *messageSerializer
	disconnected             bool
	onDisconnectListeners    []DisconnectFunc
	onRoomLeaveListeners     []LeaveRoomFunc
	onErrorListeners         []ErrorFunc
	onPingListeners          []PingFunc
	onPongListeners          []PongFunc
	onNativeMessageListeners []NativeMessageFunc
	onEventListeners         map[string][]MessageFunc
	started                  bool
}

// NewWSClient 创建websocket 客户端
func NewWSClient(evtMessagePrefix []byte) *WSClient {
	return &WSClient{
		messageType:              websocket.TextMessage,
		readTimeout:              10 * time.Second,
		WriteTimeout:             10 * time.Second,
		PingPeriod:               10 * time.Second,
		evtMessagePrefix:         evtMessagePrefix,
		messageSerializer:        newMessageSerializer(evtMessagePrefix),
		onDisconnectListeners:    make([]DisconnectFunc, 0),
		onRoomLeaveListeners:     make([]LeaveRoomFunc, 0),
		onErrorListeners:         make([]ErrorFunc, 0),
		onNativeMessageListeners: make([]NativeMessageFunc, 0),
		onEventListeners:         make(map[string][]MessageFunc, 0),
		onPongListeners:          make([]PongFunc, 0),
		started:                  false,
	}
}

// Connect 连接服务器
func (c *WSClient) Connect(url string, jar *http.CookieJar) error {
	var err error

	Dialer := websocket.Dialer{Jar: *jar}
	header := http.Header{}
	c.ws, _, err = Dialer.Dial(url, header)
	if err != nil {
		return err
	}

	return nil
}

// Emit 发送消息
func (c *WSClient) Emit(event string, data interface{}) error {
	var message []byte
	var err error

	message, err = c.messageSerializer.serialize(event, data)
	if err != nil {
		return err
	}

	err = c.ws.WriteMessage(websocket.TextMessage, message)
	if err != nil {
		c.FireOnError(err)
	}

	return err
}

// On 监听事件
func (c *WSClient) On(event string, cb MessageFunc) {
	if c.onEventListeners[event] == nil {
		c.onEventListeners[event] = make([]MessageFunc, 0)
	}

	c.onEventListeners[event] = append(c.onEventListeners[event], cb)
}

// OnMessage 监听普通的message事件
func (c *WSClient) OnMessage(cb NativeMessageFunc) {
	c.onNativeMessageListeners = append(c.onNativeMessageListeners, cb)
}

// Wait starts the pinger and the messages reader,
// it's named as "Wait" because it should be called LAST,
// after the "On" events IF server's `Upgrade` is used,
// otherise you don't have to call it because the `Handler()` does it automatically.
func (c *WSClient) Wait() {
	if c.started {
		return
	}
	c.started = true
	// start the ping
	c.startPinger()

	// start the messages reader
	c.startReader()
}

func (c *WSClient) startReader() {
	conn := c.ws
	hasReadTimeout := c.readTimeout > 0

	conn.SetPongHandler(func(s string) error {
		if hasReadTimeout {
			conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		}
		//fire all OnPong methods
		go c.fireOnPong()

		return nil
	})

	defer func() {
		c.Disconnect()
	}()

	for {
		if hasReadTimeout {
			// set the read deadline based on the configuration
			conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				c.FireOnError(err)
			}
			break
		} else {
			c.messageReceived(data)
		}

	}

}

// messageReceived checks the incoming message and fire the nativeMessage listeners or the event listeners (ws custom message)
func (c *WSClient) messageReceived(data []byte) {

	if bytes.HasPrefix(data, c.evtMessagePrefix) {
		//it's a custom ws message
		receivedEvt := c.messageSerializer.getWebsocketCustomEvent(data)
		listeners, ok := c.onEventListeners[string(receivedEvt)]
		if !ok || len(listeners) == 0 {
			return // if not listeners for this event exit from here
		}

		customMessage, err := c.messageSerializer.deserialize(receivedEvt, data)
		if customMessage == nil || err != nil {
			return
		}

		for i := range listeners {
			if fn, ok := listeners[i].(func()); ok { // its a simple func(){} callback
				fn()
			} else if fnString, ok := listeners[i].(func(string)); ok {

				if msgString, is := customMessage.(string); is {
					fnString(msgString)
				} else if msgInt, is := customMessage.(int); is {
					// here if server side waiting for string but client side sent an int, just convert this int to a string
					fnString(strconv.Itoa(msgInt))
				}

			} else if fnInt, ok := listeners[i].(func(int)); ok {
				fnInt(customMessage.(int))
			} else if fnBool, ok := listeners[i].(func(bool)); ok {
				fnBool(customMessage.(bool))
			} else if fnBytes, ok := listeners[i].(func([]byte)); ok {
				fnBytes(customMessage.([]byte))
			} else {
				listeners[i].(func(interface{}))(customMessage)
			}

		}
	} else {
		// it's native websocket message
		for i := range c.onNativeMessageListeners {
			c.onNativeMessageListeners[i](data)
		}
	}
}

// Disconnect 断开连接
func (c *WSClient) Disconnect() error {
	if c == nil || c.disconnected {
		return ErrAlreadyDisconnected
	}

	return c.ws.Close()
}

// OnDisconnect 断开事件
func (c *WSClient) OnDisconnect(cb DisconnectFunc) {
	c.onDisconnectListeners = append(c.onDisconnectListeners, cb)
}

// OnError 错误事件
func (c *WSClient) OnError(cb ErrorFunc) {
	c.onErrorListeners = append(c.onErrorListeners, cb)
}

// OnPing ping事件
func (c *WSClient) OnPing(cb PingFunc) {
	c.onPingListeners = append(c.onPingListeners, cb)
}

// OnPong pong事件
func (c *WSClient) OnPong(cb PongFunc) {
	c.onPongListeners = append(c.onPongListeners, cb)
}

// FireOnError 触发错误
func (c *WSClient) FireOnError(err error) {
	for _, cb := range c.onErrorListeners {
		cb(err)
	}
}

func (c *WSClient) fireOnPing() {
	// fire the onPingListeners
	for i := range c.onPingListeners {
		c.onPingListeners[i]()
	}
}

func (c *WSClient) fireOnPong() {
	// fire the onPongListeners
	for i := range c.onPongListeners {
		c.onPongListeners[i]()
	}
}

func (c *WSClient) fireDisconnect() {
	for i := range c.onDisconnectListeners {
		c.onDisconnectListeners[i]()
	}
}

func (c *WSClient) startPinger() {

	// this is the default internal handler, we just change the writeWait because of the actions we must do before
	// the server sends the ping-pong.

	pingHandler := func(message string) error {
		err := c.ws.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(WriteWait))
		if err == websocket.ErrCloseSent {
			return nil
		} else if e, ok := err.(net.Error); ok && e.Temporary() {
			return nil
		}
		return err
	}

	c.ws.SetPingHandler(pingHandler)

	go func() {
		for {
			// using sleep avoids the ticker error that causes a memory leak
			time.Sleep(c.PingPeriod)
			if c.disconnected {
				// verifies if already disconected
				break
			}
			//fire all OnPing methods
			c.fireOnPing()
			// try to ping the client, if failed then it disconnects
			err := c.Write(websocket.PingMessage, []byte{})
			if err != nil {
				// must stop to exit the loop and finish the go routine
				break
			}
		}
	}()
}

// Write writes a raw websocket message with a specific type to the client
// used by ping messages and any CloseMessage types.
func (c *WSClient) Write(websocketMessageType int, data []byte) error {
	// for any-case the app tries to write from different goroutines,
	// we must protect them because they're reporting that as bug...
	c.writerMu.Lock()
	if writeTimeout := c.WriteTimeout; writeTimeout > 0 {
		// set the write deadline based on the configuration
		c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	}

	// .WriteMessage same as NextWriter and close (flush)
	err := c.ws.WriteMessage(websocketMessageType, data)
	c.writerMu.Unlock()
	if err != nil {
		// if failed then the connection is off, fire the disconnect
		c.Disconnect()
	}

	return err
}
