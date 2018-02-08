package signalr

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/whatmatrix/signalr/hubs"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

// MessageReader is the interface that wraps ReadMessage.
//
// ReadMessage is defined at
// https://godoc.org/github.com/gorilla/websocket#Conn.ReadMessage
//
// At a high level, it reads messages and returns:
//  - the type of message read
//  - the bytes that were read
//  - any errors encountered during reading the message
type MessageReader interface {
	ReadMessage() (messageType int, p []byte, err error)
}

// JSONWriter is the interface that wraps WriteJSON.
//
// WriteJSON is defined at
// https://godoc.org/github.com/gorilla/websocket#Conn.WriteJSON
//
// At a high level, it writes a structure to the underlying websocket and
// returns any error that was encountered during the write operation.
type JSONWriter interface {
	WriteJSON(v interface{}) error
}

// WebsocketConn is a combination of MessageReader and JSONWriter. It is used to
// provide an interface to objects that can read from and write to a websocket
// connection.
type WebsocketConn interface {
	MessageReader
	JSONWriter
}

// Message represents a message sent from the server to the persistent websocket
// connection.
type Message struct {
	// message id, present for all non-KeepAlive messages
	C string

	// an array containing actual data
	M []hubs.ClientMsg

	// indicates that the transport was initialized (a.k.a. init message)
	S int

	// groups token – an encrypted string representing group membership
	G string
}

type RawClientMessage struct {
	I int

	// the name of the hub
	H string

	// the name of the method
	M string

	// arguments (an array, can be empty if the method does not have any
	// parameters)
	A []json.RawMessage

	// state – a dictionary containing additional custom data (optional)
	S json.RawMessage `json:",omitempty"`
}

type RawMessage struct {
        I int
	C string
	M []RawClientMessage
	E string
	R json.RawMessage
	H json.RawMessage // could be bool or string depending on a message type
	D json.RawMessage
	T json.RawMessage
	S json.RawMessage
}

// Scheme represents a type of transport scheme. For the purposes of this
// project, we only provide constants for schemes relevant to HTTP and
// websockets.
type Scheme string

const (
	// HTTPS is the literal string, "https".
	HTTPS Scheme = "https"

	// HTTP is the literal string, "http".
	HTTP Scheme = "http"

	// WSS is the literal string, "wss".
	WSS Scheme = "wss"

	// WS is the literal string, "ws".
	WS Scheme = "ws"
)

// Client represents a SignlR client. It manages connections so that the caller
// doesn't have to.
type Client struct {
	// The host providing the SignalR service.
	Host string

	// The relative path where the SignalR service is provided.
	Endpoint string

	// The websockets protocol version.
	Protocol string

	ConnectionData string

	// The HTTPClient used to initialize the websocket connection.
	HTTPClient *http.Client

	// An optional setting to provide a non-default TLS configuration to use
	// when connecting to the websocket.
	TLSClientConfig *tls.Config

	// Either HTTPS or HTTP.
	Scheme Scheme

	// The maximum number of times to re-attempt a negotiation.
	MaxNegotiateRetries int

	// The maximum number of times to re-attempt a connection.
	MaxConnectRetries int

	// The maximum number of times to re-attempt a reconnection.
	MaxReconnectRetries int

	// The maximum number of times to re-attempt a start command.
	MaxStartRetries int

	// The time to wait before retrying, in the event that an error occurs
	// when contacting the SignalR service.
	RetryWaitDuration time.Duration

	// This is the connection token set during the negotiate phase of the
	// protocol and used to uniquely identify the connection to the server
	// in all subsequent phases of the connection.
	ConnectionToken string

	// This is the ID of the connection. It is set during the negotiate
	// phase and then ignored by all subsequent steps.
	ConnectionID string

	// Header values that should be applied to all HTTP requests.
	Headers map[string]string

	// This value is not part of the SignalR protocol. If this value is set,
	// it will be used in debug messages.
	CustomID string

	// This field holds a struct that can read messages from and write JSON
	// objects to a websocket. In practice, this is simply a raw websocket
	// connection that results from a successful connection to the SignalR
	// server.
	conn    WebsocketConn
	connMux sync.Mutex

	close chan struct{}
}

func debugEnabled() bool {
	v := os.Getenv("DEBUG")
	return v != ""
}

func debugMessage(msg string, v ...interface{}) {
	if debugEnabled() {
		log.Printf(msg, v...)
	}
}

func prefixedID(ID string) string {
	if ID == "" {
		return ""
	}

	return "[" + ID + "] "
}

// Conditionally encrypt the traffic depending on the initial
// connection's encryption.
func setWebsocketURLScheme(u *url.URL, httpScheme Scheme) {
	if httpScheme == HTTPS {
		u.Scheme = string(WSS)
	} else {
		u.Scheme = string(WS)
	}
}

func makeURL(command string, c *Client) (u url.URL) {
	// Set the host.
	u.Host = c.Host

	// Set the first part of the path.
	u.Path = c.Endpoint

	// Create parameters.
	params := url.Values{}

	// Add shared parameters.
	params.Set("connectionData", c.ConnectionData)
	params.Set("clientProtocol", c.Protocol)

	// Set the connectionToken.
	if c.ConnectionToken != "" {
		params.Set("connectionToken", c.ConnectionToken)
	}

	switch command {
	case "negotiate":
		u.Scheme = string(c.Scheme)
		u.Path += "/negotiate"
	case "connect":
		setWebsocketURLScheme(&u, c.Scheme)
		params.Set("transport", "webSockets")
		u.Path += "/connect"
	case "reconnect":
		setWebsocketURLScheme(&u, c.Scheme)
		params.Set("transport", "webSockets")
		u.Path += "/reconnect"
	case "start":
		u.Scheme = string(c.Scheme)
		params.Set("transport", "webSockets")
		u.Path += "/start"
	}

	// Set the parameters.
	u.RawQuery = params.Encode()

	return
}

func prepareRequest(url string, headers map[string]string) (req *http.Request, err error) {
	req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		err = errors.Wrap(err, "get request failed")
		return
	}

	// Add all header values.
	for k, v := range headers {
		req.Header.Add(k, v)
	}

	return
}

func (c *Client) processNegotiateResponse(body io.ReadCloser) (err error) {
	defer func() {
		derr := body.Close()
		if derr != nil {
			err = errors.Wrapf(err, "close body failed | %v", derr)
		}
	}()

	var data []byte
	data, err = ioutil.ReadAll(body)
	if err != nil {
		err = errors.Wrap(err, "read failed")
		return
	}

	// Create a struct to allow parsing of the response object.
	parsed := struct {
		URL                     string `json:"Url"`
		ConnectionToken         string
		ConnectionID            string `json:"ConnectionId"`
		KeepAliveTimeout        float64
		DisconnectTimeout       float64
		ConnectionTimeout       float64
		TryWebSockets           bool
		ProtocolVersion         string
		TransportConnectTimeout float64
		LongPollDelay           float64
	}{}
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failed")
		return
	}

	// Set the connection token and ID.
	c.ConnectionToken = parsed.ConnectionToken
	c.ConnectionID = parsed.ConnectionID

	// Update the protocol version.
	c.Protocol = parsed.ProtocolVersion

	// Set the SignalR endpoint.
	c.Endpoint = parsed.URL

	return
}

// Negotiate implements the negotiate step of the SignalR connection sequence.
func (c *Client) Negotiate() (err error) {
	// Reset the connection token in case it has been set.
	c.ConnectionToken = ""

	// Make a "negotiate" URL.
	u := makeURL("negotiate", c)

	// Make a flag to use for indicating whether or not an error occurred.
	errOccurred := false

	for i := 0; i < c.MaxNegotiateRetries; i++ {
		var req *http.Request
		req, err = prepareRequest(u.String(), c.Headers)
		if err != nil {
			err = errors.Wrap(err, "request preparation failed")
			return
		}

		// Perform the request.
		var resp *http.Response
		resp, err = c.HTTPClient.Do(req)
		if err != nil {
			err = errors.Wrap(err, "request failed")
			return
		}

		// Perform operations specific to the status code.
		switch resp.StatusCode {
		case 200:
			// Everything worked, so do nothing.
		case 503:
			fallthrough
		case 524:
			fallthrough
		default:
			err = errors.Errorf("request failed: %s", resp.Status)
			debugMessage("%snegotiate: retrying after %s", prefixedID(c.CustomID), resp.Status)
			errOccurred = true
			time.Sleep(c.RetryWaitDuration)
			continue
		}

		err = c.processNegotiateResponse(resp.Body)

		if errOccurred {
			// If an error occurred earlier, and yet we got here,
			// then we want to let the user know that the
			// negotiation successfully recovered.
			debugMessage("%sthe negotiate retry was successful", prefixedID(c.CustomID))
		}

		return
	}

	if errOccurred {
		debugMessage("%sthe negotiate retry was unsuccessful", prefixedID(c.CustomID))
	}

	return
}

func makeHeader(c *Client) (header http.Header) {
	// Create a header object that contains any cookies that have been set
	// in prior requests.
	header = make(http.Header)

	// If no client is specified, return an empty header.
	if c == nil {
		return
	}

	// Add cookies if they are set.
	if c.HTTPClient != nil && c.HTTPClient.Jar != nil {
		// Make a negotiate URL so we can look up the cookie that was
		// set on the negotiate request.
		nu := makeURL("negotiate", c)
		cookies := ""
		for _, v := range c.HTTPClient.Jar.Cookies(&nu) {
			if cookies == "" {
				cookies += v.Name + "=" + v.Value
			} else {
				cookies += "; " + v.Name + "=" + v.Value
			}
		}

		if cookies != "" {
			header.Add("Cookie", cookies)
		}
	}

	// Add all the other header values specified by the user.
	for k, v := range c.Headers {
		header.Add(k, v)
	}

	return
}

func (c *Client) xconnect(url string, isReconnect bool) (conn *websocket.Conn, err error) {
	// Create a dialer that uses the supplied TLS client configuration.
	dialer := &websocket.Dialer{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: c.TLSClientConfig,
	}

	// Prepare a header to be used when dialing to the service.
	header := makeHeader(c)

	var retryCount int
	if isReconnect {
		retryCount = c.MaxReconnectRetries
	} else {
		retryCount = c.MaxConnectRetries
	}

	// Perform the connection in a retry loop.
	for i := 0; i < retryCount; i++ {
		var resp *http.Response
		conn, resp, err = dialer.Dial(url, header)
		if err == nil {
			// If there was no error, break out of the retry loop.
			break
		}

		// Verify that a response accompanies the error.
		if resp == nil {
			err = errors.Wrapf(err, "empty response, retry %d", i)

			// If no response is set, then wait and retry.
			time.Sleep(c.RetryWaitDuration)
			continue
		}

		// According to documentation at
		// https://godoc.org/github.com/gorilla/websocket#Dialer.Dial
		// ErrBadHandshake is the only error returned. Details reside in
		// the response, so that's how we process this error.
		err = errors.Wrapf(err, "%v, retry %d", resp.Status, i)

		// Handle any specific errors.
		switch resp.StatusCode {
		case 503:
			// Wait and retry.
			time.Sleep(c.RetryWaitDuration)
			continue
		default:
			// Return in the event that no specific error was
			// encountered.
			return
		}
	}

	return
}

// Connect implements the connect step of the SignalR connection sequence.
func (c *Client) Connect() (conn *websocket.Conn, err error) {
	// Example connect URL:
	// https://socket.bittrex.com/signalr/connect?
	//   transport=webSockets&
	//   clientProtocol=1.5&
	//   connectionToken=<token>&
	//   connectionData=%5B%7B%22name%22%3A%22corehub%22%7D%5D&
	//   tid=5
	// -> returns connection ID. (e.g.: d-F2577E41-B,0|If60z,0|If600,1)

	// Create the URL.
	u := makeURL("connect", c)

	// Perform the connection.
	conn, err = c.xconnect(u.String(), false)
	if err != nil {
		err = errors.Wrap(err, "xconnect failed")
	}

	return
}

func (c *Client) processStartResponse(body io.ReadCloser, conn WebsocketConn) (err error) {
	defer func() {
		derr := body.Close()
		if derr != nil {
			err = errors.Wrapf(err, "close body failed | %v", derr)
		}
	}()

	var data []byte
	data, err = ioutil.ReadAll(body)
	if err != nil {
		err = errors.Wrap(err, "read failed")
		return
	}

	// Create an anonymous struct to parse the response.
	parsed := struct{ Response string }{}
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failed")
		return
	}

	// Confirm the server response is what we expect.
	if parsed.Response != "started" {
		err = errors.Errorf("start response is not 'started': %s", parsed.Response)
		return
	}

	// Wait for the init message.
	var t int
	var p []byte
	t, p, err = conn.ReadMessage()
	if err != nil {
		err = errors.Wrap(err, "message read failed")
		return
	}

	// Verify the correct response type was received.
	if t != websocket.TextMessage {
		err = errors.Errorf("unexpected websocket control type: %d", t)
		return
	}

	// Extract the server message.
	var pcm Message
	err = json.Unmarshal(p, &pcm)
	if err != nil {
		err = errors.Wrap(err, "json unmarshal failed")
		return
	}

	serverInitialized := 1
	if pcm.S != serverInitialized {
		err = errors.Errorf("unexpected S value received from server: %d | message: %s", pcm.S, string(p))
		return
	}

	// Since we got to this point, the connection is successful. So we set
	// the connection for the client.
	c.conn = conn
	return
}

// SetConn changes the underlying websocket connection to the specified
// connection. This is done using a mutex to wait until existing read operations
// have completed.
func (c *Client) SetConn(conn WebsocketConn) {
	c.connMux.Lock()
	c.conn = conn
	c.connMux.Unlock()
}

// Conn returns the underlying websocket connection.
func (c *Client) Conn() WebsocketConn {
	return c.conn
}

// Start implements the start step of the SignalR connection sequence.
func (c *Client) Start(conn WebsocketConn) (err error) {
	if conn == nil {
		return errors.New("connection is nil")
	}

	u := makeURL("start", c)

	var req *http.Request
	req, err = prepareRequest(u.String(), c.Headers)
	if err != nil {
		err = errors.Wrap(err, "request preparation failed")
		return
	}

	// Perform the request in a retry loop.
	var resp *http.Response
	for i := 0; i < c.MaxStartRetries; i++ {
		resp, err = c.HTTPClient.Do(req)

		// Exit the retry loop if the request was successful.
		if err == nil {
			break
		}

		// If the request was unsuccessful, wrap the error, sleep, and
		// then retry.
		err = errors.Wrapf(err, "request failed (%d)", i)

		// Wait and retry.
		time.Sleep(c.RetryWaitDuration)
	}

	// If an error occurred on the last retry, then return.
	if err != nil {
		err = errors.Wrap(err, "all request retries failed")
		return
	}

	if resp == nil {
		err = errors.New("response is nil")
		return
	}

	err = c.processStartResponse(resp.Body, conn)
	return
}

// Reconnect implements the reconnect step of the SignalR connection sequence.
func (c *Client) Reconnect() (conn *websocket.Conn, err error) {
	// Note from
	// https://blog.3d-logic.com/2015/03/29/signalr-on-the-wire-an-informal-description-of-the-signalr-protocol/
	// Once the channel is set up there are no further HTTP requests until
	// the client is stopped (the abort request) or the connection was lost
	// and the client tries to re-establish the connection (the reconnect
	// request).

	// Example reconnect URL:
	// https://socket.bittrex.com/signalr/reconnect?
	//   transport=webSockets&
	//   messageId=d-F2577E41-B%2C0%7CIf60z%2C0%7CIf600%2C1&
	//   clientProtocol=1.5&
	//   connectionToken=<same-token-as-above>&
	//   connectionData=%5B%7B%22name%22%3A%22corehub%22%7D%5D&
	//   tid=7
	// Note: messageId matches connection ID returned from the connect request

	// Create the URL.
	u := makeURL("reconnect", c)

	// Perform the reconnection.
	conn, err = c.xconnect(u.String(), true)
	if err != nil {
		err = errors.Wrap(err, "xconnect failed")
		return
	}

	// Once complete, set the new connection for this client.
	c.conn = conn

	return
}

// Run connects to the host and performs the websocket initialization routines
// that are part of the SignalR specification. It returns channels that:
//  - receive messages from the websocket connection
//  - receive errors encountered while processing the weblocket connection
func (c *Client) Run() (msgCh chan RawMessage, errCh chan error, err error) {
	errCh = make(chan error)
	msgCh = make(chan RawMessage)

	// Make a channel that is used to indicate that the connection
	// initialization functions have completed or errored out.
	done := make(chan bool)

	go func() {
		defer func() {
			// Once this goroutine returns, indicate that it has
			// finished executing.
			done <- true
		}()

		err = c.Negotiate()
		if err != nil {
			err = errors.Wrap(err, "negotiate failed")
			return
		}

		var conn *websocket.Conn
		conn, err = c.Connect()
		if err != nil {
			err = errors.Wrap(err, "connect failed")
			return
		}

		err = c.Start(conn)
		if err != nil {
			err = errors.Wrap(err, "start failed")
			return
		}

		// Start the read message loop.
		go c.ReadMessages(msgCh, errCh)
	}()

	// Wait for initialization goroutine to complete.
	<-done

	return
}

func (c *Client) attemptReconnect(msgCh chan RawMessage, errCh chan error) (ok bool) {
	// Attempt to reconnect in a retry loop.
	reconnected := false
	for i := 0; i < c.MaxReconnectRetries; i++ {
		debugMessage("%sattempting to reconnect...", prefixedID(c.CustomID))

		_, err := c.Reconnect()
		if err != nil {
			// Ignore the value of the error and just continue.
			continue
		}

		debugMessage("%sreconnected successfully", prefixedID(c.CustomID))
		reconnected = true
		break
	}

	// If the reconnect attempt succeeded, ignore the error. Since we are
	// still within the readmessage loop, the next call to the WebSocket's
	// ReadMessage() function will use the new c.conn connection, so we
	// don't have to do any more connection repair.
	if reconnected {
		ok = true
	}

	return
}

func errCode(err error) (code int) {
	re := regexp.MustCompile("[0-9]+")
	s := re.FindString(err.Error())

	var e error
	code, e = strconv.Atoi(s)
	if e != nil {
		// -1 is not a valid error code, so we use this, rather than
		// introducing the need for another error handler on the caller
		// of this function.
		code = -1
	}
	return
}

func (c *Client) processReadMessagesError(err error, msgCh chan RawMessage, errCh chan error) (ok bool) {
	// Handle various types of errors.
	// https://tools.ietf.org/html/rfc6455#section-7.4.1
	code := errCode(err)
	switch code {
	case 1000:
		// normal closure
		fallthrough
	case 1001:
		// going away
		fallthrough
	case 1006:
		// abnormal closure
		ok = c.attemptReconnect(msgCh, errCh)
	default:
		errCh <- err
	}

	return
}

func processReadMessagesMessage(p []byte, msgs chan RawMessage, errs chan error) {
	// Ignore KeepAlive messages.
	//fmt.Println("received: ", string(p))
	if len(p) == 2 && p[0] == '{' && p[1] == '}' {
		return
	}

	var msg RawMessage
	err := json.Unmarshal(p, &msg)
	if err != nil {
		fmt.Println("Json unmarshal fail")
		err = errors.Wrap(err, "json unmarshal failed")
		errs <- err
		return
	}

	msgs <- msg
}

func (c *Client) readMessage(msgCh chan RawMessage, errCh chan error) (ok bool) {
	c.connMux.Lock()

	// Set up a channel to receive signals from the Client.readMessage
	// function as well as the goroutine that does the actual
	// Client.conn.read. This way, the unlock operation is synchronized.
	unlockCh := make(chan bool, 2)
	go func() {
		<-unlockCh
		<-unlockCh
		c.connMux.Unlock()
	}()

	// Set the ok flag to true to indicate that more messages can/should be
	// read. Set the flag to false later on if this is no longer the case.
	ok = true

	// Prepare channels for the select statement later.
	pCh := make(chan []byte)
	errs := make(chan error)

	// Wait for a message.
	go func() {
		_, p, err := c.conn.ReadMessage()
		if err != nil {
			errs <- err
		} else {
			pCh <- p
		}

		// Send a signal that the inner function is done processing.
		unlockCh <- true
	}()

	select {
	case err := <-errs:
		ok = c.processReadMessagesError(err, msgCh, errCh)
	case p := <-pCh:
		processReadMessagesMessage(p, msgCh, errCh)
	case <-c.close:
		ok = false
	}

	// Send a signal that the outer function is done processing.
	unlockCh <- true

	return
}

// ReadMessages processes WebSocket messages from the underlying websocket
// connection. When a message is processed, it is passed along the msgCh
// channel. When an error ocurrs, it is sent along the errCh channel.
func (c *Client) ReadMessages(msgCh chan RawMessage, errCh chan error) {
	for {
		if !c.readMessage(msgCh, errCh) {
			return
		}
	}
}

// Send sends a message to the websocket connection.
func (c *Client) Send(m hubs.ClientMsg) (err error) {
	c.connMux.Lock()
	defer func() {
		c.connMux.Unlock()
	}()

	// Verify a connection has been created.
	if c.conn == nil {
		err = errors.New("send: connection not set")
		return
	}

	// Write the message.
	err = c.conn.WriteJSON(m)
	if err != nil {
		err = errors.Wrap(err, "json write failed")
	}

	return
}

// Close sends a signal to the loop reading WebSocket messages to indicate that
// the loop should terminate.
func (c *Client) Close() {
	c.close <- struct{}{}
}

// New creates and initializes a SignalR client.
func New(host, protocol, endpoint, connectionData string) (c *Client) {
	// Create the client.
	c = new(Client)

	// Set the parameters that were passed in.
	c.Host = host
	c.Protocol = protocol
	c.Endpoint = endpoint
	c.ConnectionData = connectionData
	c.close = make(chan struct{})

	c.HTTPClient = new(http.Client)
	c.Headers = make(map[string]string)

	// Default to using a secure scheme.
	c.Scheme = HTTPS

	// Set the default max number of negotiate retries.
	c.MaxNegotiateRetries = 5

	// Set the default max number of connect retries.
	c.MaxConnectRetries = 5

	// Set the default max number of reconnect retries.
	c.MaxReconnectRetries = 5

	// Set the default max number of start retries.
	c.MaxStartRetries = 5

	// Set the default sleep duration between retries.
	c.RetryWaitDuration = 1 * time.Minute

	return
}
