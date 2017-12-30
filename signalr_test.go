package signalr_test

import (
	"crypto/x509"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/carterjones/helpers/trace"
	"github.com/carterjones/signalr"
	"github.com/gorilla/websocket"
)

func equals(tb testing.TB, id string, exp, act interface{}) {
	if !reflect.DeepEqual(exp, act) {
		_, file, line, _ := runtime.Caller(1)
		tb.Errorf("\n\033[31m%s:%d (%s):\n\n\texp: %#v\n\tgot: %#v\033[39m\n\n",
			filepath.Base(file), line, id, exp, act)
	}
}

func notNil(tb testing.TB, id string, act interface{}) {
	if act == nil {
		_, file, line, _ := runtime.Caller(1)
		tb.Errorf("\n\033[31m%s:%d (%s):\n\n\texp: a non-nil value\n\tgot: %#v\033[39m\n\n",
			filepath.Base(file), line, id, act)
	}
}

// Note: this is largely derived from
// https://github.com/golang/go/blob/1c69384da4fb4a1323e011941c101189247fea67/src/net/http/response_test.go#L915-L940
func errMatches(tb testing.TB, id string, err error, wantErr interface{}) {
	if err == nil {
		if wantErr == nil {
			return
		}

		if sub, ok := wantErr.(string); ok {
			tb.Errorf("%s: unexpected success; want error with substring %q", id, sub)
			return
		}

		tb.Errorf("%s: unexpected success; want error %v", id, wantErr)
		return
	}

	if wantErr == nil {
		tb.Errorf("%s: %v; want success", id, err)
		return
	}

	if sub, ok := wantErr.(string); ok {
		if strings.Contains(err.Error(), sub) {
			return
		}
		tb.Errorf("%s, error = %v; want an error with substring %q", id, err, sub)
		return
	}

	if err == wantErr {
		return
	}

	tb.Errorf("%s: %v; want %v", id, err, wantErr)
}

func hostFromServerURL(url string) (host string) {
	host = strings.TrimPrefix(url, "https://")
	host = strings.TrimPrefix(host, "http://")
	return
}

func newTestServer(fn http.HandlerFunc, tls bool) (ts *httptest.Server) {
	if tls {
		// Create the server.
		ts = httptest.NewTLSServer(fn)

		// Save the testing certificate to the TLS client config.
		//
		// I'm not sure why ts.TLS doesn't contain certificate
		// information. However, this seems to make the testing TLS
		// certificate be trusted by the client.
		ts.TLS.RootCAs = x509.NewCertPool()
		ts.TLS.RootCAs.AddCert(ts.Certificate())
	} else {
		// Create the server.
		ts = httptest.NewServer(fn)
	}

	return
}

func newTestClient(protocol, endpoint, connectionData string, ts *httptest.Server) (c *signalr.Client) {
	// Prepare a SignalR client.
	c = signalr.New(hostFromServerURL(ts.URL), protocol, endpoint, connectionData)
	c.HTTPClient = ts.Client()

	// Save the TLS config in case this is using TLS.
	if ts.TLS != nil {
		c.TLSClientConfig = ts.TLS
		c.Scheme = signalr.HTTPS
	} else {
		c.Scheme = signalr.HTTP
	}

	return
}

func negotiate(w http.ResponseWriter, r *http.Request) {
	_, err := w.Write([]byte(`{"ConnectionToken":"hello world","ConnectionId":"1234-ABC","URL":"/signalr","ProtocolVersion":"1337"}`))
	if err != nil {
		trace.Error(err)
		return
	}
}

func connect(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		trace.Error(err)
		return
	}

	go func() {
		for {
			var msgType int
			var bs []byte
			msgType, bs, err = c.ReadMessage()
			if err != nil {
				trace.Error(err)
				return
			}

			log.Println(msgType, string(bs))
		}
	}()

	go func() {
		for {
			err = c.WriteMessage(websocket.TextMessage, []byte(`{"S":1}`))
			if err != nil {
				trace.Error(err)
				return
			}
		}
	}()
}

func start(w http.ResponseWriter, r *http.Request) {
	_, err := w.Write([]byte(`{"Response":"started"}`))
	if err != nil {
		trace.Error(err)
	}
}

func throw503Error(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("503 error"))
}

func throw123Error(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(123)
	w.Write([]byte("123 error"))
}

func throwMalformedStatusCodeError(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(9001)
	w.Write([]byte("malformed status code"))
}

func TestClient_Negotiate(t *testing.T) {
	cases := map[string]struct {
		fn      http.HandlerFunc
		in      signalr.Client
		TLS     bool
		exp     signalr.Client
		wantErr string
	}{
		"successful http": {
			fn: negotiate,
			in: signalr.Client{
				Protocol:       "1337",
				Endpoint:       "/signalr",
				ConnectionData: "all the data",
			},
			TLS: false,
			exp: signalr.Client{
				Protocol:        "1337",
				Endpoint:        "/signalr",
				ConnectionToken: "hello+world",
				ConnectionID:    "1234-ABC",
			},
		},
		"successful https": {
			fn: negotiate,
			in: signalr.Client{
				Protocol:       "1337",
				Endpoint:       "/signalr",
				ConnectionData: "all the data",
			},
			TLS: true,
			exp: signalr.Client{
				Protocol:        "1337",
				Endpoint:        "/signalr",
				ConnectionToken: "hello+world",
				ConnectionID:    "1234-ABC",
			},
		},
		"503 error": {
			fn:      throw503Error,
			wantErr: "503 Service Unavailable",
		},
		"default error": {
			fn:      throw123Error,
			wantErr: "123 status code",
		},
		"failed get request": {
			fn:      throwMalformedStatusCodeError,
			wantErr: "malformed HTTP status code",
		},
		"invalid json": {
			fn: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("invalid json"))
			},
			wantErr: "invalid character 'i' looking for beginning of value",
		},
	}

	for id, tc := range cases {
		// Create a test server.
		ts := newTestServer(http.HandlerFunc(tc.fn), tc.TLS)
		defer ts.Close()

		// Create a test client.
		c := newTestClient(tc.in.Protocol, tc.in.Endpoint, tc.in.ConnectionData, ts)

		// Set the wait time to milliseconds.
		c.RetryWaitDuration = 1 * time.Millisecond

		// Perform the negotation.
		err := c.Negotiate()

		// Make sure the error matches the expected error.
		if tc.wantErr != "" {
			errMatches(t, id, err, tc.wantErr)
		}

		// Validate the things we expect.
		equals(t, id, tc.exp.ConnectionToken, c.ConnectionToken)
		equals(t, id, tc.exp.ConnectionID, c.ConnectionID)
		equals(t, id, tc.exp.Protocol, c.Protocol)
		equals(t, id, tc.exp.Endpoint, c.Endpoint)
	}
}

func TestClient_Connect(t *testing.T) {
}

func TestClient_Start(t *testing.T) {
}

func TestClient_Reconnect(t *testing.T) {
}

func TestClient_Init(t *testing.T) {
	ts := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/negotiate") {
			negotiate(w, r)
		} else if strings.Contains(r.URL.Path, "/connect") {
			connect(w, r)
		} else if strings.Contains(r.URL.Path, "/start") {
			start(w, r)
		} else {
			log.Println("url:", r.URL)
		}
	}), true)
	defer ts.Close()

	c := newTestClient("1.5", "/signalr", "all the data", ts)

	// Initialize the client.
	err := c.Init()
	if err != nil {
		log.Panic(err)
	}

	// TODO: literally any form of validatation
	// TODO: check for specific errors
}

func TestClient_Send(t *testing.T) {
}

func TestClient_Messages(t *testing.T) {
}

func TestNew(t *testing.T) {
	// Define parameter values.
	host := "test-host"
	protocol := "test-protocol"
	endpoint := "test-endpoint"
	connectionData := "test-connection-data"

	// Create the client.
	c := signalr.New(host, protocol, endpoint, connectionData)

	// Validate values were set up properly.
	equals(t, "host", host, c.Host)
	equals(t, "protocol", protocol, c.Protocol)
	equals(t, "endpoint", endpoint, c.Endpoint)
	equals(t, "connection data", connectionData, c.ConnectionData)
	equals(t, "http client", new(http.Client), c.HTTPClient)
	equals(t, "scheme", signalr.HTTPS, c.Scheme)
	equals(t, "max negotiate retries", 5, c.MaxNegotiateRetries)
	equals(t, "retry wait duration", 1*time.Minute, c.RetryWaitDuration)
	notNil(t, "messages", c.Messages())
}
