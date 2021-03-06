package autosolve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	ee "github.com/jiyeyuran/go-eventemitter"
	"google.golang.org/protobuf/proto"

	"github.com/kylin-public/kylin-autosolve-client-go/gowebsocket"
	"github.com/kylin-public/kylin-autosolve-client-go/protocol"
)

var autosolveURL = "wss://autosolve-ws.kylinbot.io/ws"
var retryIntervals = []int{1000, 2000, 4000, 8000, 16000}

// NotificationTaskResult is the event name for the task result notification
var NotificationTaskResult = "Notification-" + reflect.TypeOf(&protocol.Notification_TaskResult{}).String()

// ErrorAborted is returned in Invoke() when a call is aborted
var ErrorAborted = errors.New("autosolve: the request is aborted")

// ErrorNotConnected is returned in Invoke() when the underlying websocket is disconnected
var ErrorNotConnected = errors.New("autosolve: can't make a call when not connected")

// ErrorLogin is returned when login failed
var ErrorLogin = errors.New("autosolve: unable to log in to the server")

// ErrorNoAuthorizationInfo is returned in Start() but required authroization info is missing
var ErrorNoAuthorizationInfo = errors.New("autosolve: authroization info is missing")

// RemoteError is the errors returned from server
type RemoteError struct {
	s    string
	Code int
}

// NewRemoteError creates a new remote error
func NewRemoteError(code int, text string) error {
	return &RemoteError{text, code}
}

func (e *RemoteError) Error() string {
	return e.s
}

// Client provides a client for the kylin autosolve service
type Client struct {
	ws gowebsocket.Socket

	AccessToken      string
	ClientKey        string
	HeartBeatTimeout int

	EE            ee.IEventEmitter
	ee            ee.IEventEmitter
	nextRequestID uint64
	requests      sync.Map

	connMutex      sync.Mutex
	cancelFunc     context.CancelFunc
	retryCount     int
	loginError     bool
	ticker         *time.Ticker
	tickerChan     chan byte
	heartBeatCount int

	mutex    sync.Mutex
	started  bool
	loggedIn bool
}

// NotificationEvent is the payload object of Notification events
type NotificationEvent struct {
	Message      *protocol.Message
	Notification *protocol.Notification
	Handled      int
}

// Param contains a parameter of a POST request
type Param struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ITaskOptions is a interfaces that represents options of a CreateTaskRequest
type ITaskOptions interface{}

// InputInfo contains the additional info for the SMS-CODE challenge
type InputInfo struct {
	ID         string `json:"id,omitempty"`
	Timestamp  int64  `json:"timestamp,omitempty"`
	InputName  string `json:"input_name,omitempty"`
	InputLabel string `json:"input_label,omitempty"`
}

// TaskOptions is the options of create request
type TaskOptions struct {
	Type               string     `json:"type,omitempty"`
	SiteKey            string     `json:"site_key,omitempty"`
	Version            string     `json:"version,omitempty"`
	Enterprise         *bool      `json:"enterprise,omitempty"`
	Invisible          *bool      `json:"invisible,omitempty"`
	Action             string     `json:"action,omitempty"`
	Secret             string     `json:"secret,omitempty"`
	Challenge          string     `json:"challenge,omitempty"`
	RqData             string     `json:"rqdata,omitempty"`
	APIServer          string     `json:"api_server,omitempty"`
	ActionURL          string     `json:"action_url,omitempty"`
	Method             string     `json:"method,omitempty"`
	Params             *[]Param   `json:"params,omitempty"`
	Document           string     `json:"document,omitempty"`
	CallbackURLPattern string     `json:"callback_url_pattern,omitempty"`
	Filters            []string   `json:"filters,omitempty"`
	Info               *InputInfo `json:"info,omitempty"`
}

// CreateTaskRequest contains the basic options for a create request
type CreateTaskRequest struct {
	ChallengeType string
	URL           string
	Timeout       int
}

// GetTaskResultRequest contains the basic options for a get request
type GetTaskResultRequest struct {
	TaskID string
}

// CancelTaskRequest contains the basic options for a cancel request
type CancelTaskRequest struct {
	ChallengeType string
	TaskID        string
}

type response struct {
	Message *protocol.Message
	Error   error
}

// New creates a autosolve client
func New() *Client {
	c := &Client{
		ws:               gowebsocket.New(autosolveURL),
		EE:               ee.NewEventEmitter(),
		ee:               ee.NewEventEmitter(),
		HeartBeatTimeout: 8,
		nextRequestID:    1000,
	}

	return c
}

// SetURL sets the URL of autosolve service
func (c *Client) SetURL(url string) {
	c.ws.URL = url
}

// IsConnected returns the status of underlying websocket connection
func (c *Client) IsConnected() bool {
	return c.ws.IsConnected
}

// IsLoggedIn returns true if this client connected to server and signed in successfully
func (c *Client) IsLoggedIn() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.loggedIn
}

// IsStarted returns true if this client has been started
func (c *Client) IsStarted() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.started
}

// Start connects to kylin autosolve server and signin using provided authorization information
func (c *Client) Start() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.started {
		return nil
	}

	c.retryCount = 0
	if err := c.connect(); err != nil {
		return err
	}
	c.started = true
	return nil
}

// Stop disconnects the underlying websocket connection and abort all pending calls
func (c *Client) Stop() {
	closing := false
	(func() {
		c.mutex.Lock()
		defer c.mutex.Unlock()

		if c.started {
			closing = true
			c.started = false

			// close will emit the Abort event on the internal event emitter to abort all pending
			// calls, the only listener is installed by the Invoke() and it doesn't take any mutex,
			// should be safe.
			c.close()
		}
	})()

	if closing {
		c.EE.Emit("Closed")
	}
}

// WhenReady blocks current routine until this client is stopped or signin is completed
func (c *Client) WhenReady() error {
	return c.WhenReadyWithContext(context.Background())
}

var errorNotReady = errors.New("not ready yet")

func (c *Client) whenReady() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.started && c.loggedIn {
		return nil
	}
	if !c.started {
		return ErrorNotConnected
	}
	if c.loginError {
		return ErrorLogin
	}

	return errorNotReady
}

// WhenReadyWithContext blocks current routine until this client is stopped or signin is completed
func (c *Client) WhenReadyWithContext(ctx context.Context) error {
	result := c.whenReady()
	if result != errorNotReady {
		return result
	}

	ch := make(chan byte)
	listener := func(payload ...interface{}) {
		if len(payload) > 0 {
			if err, ok := payload[0].(error); ok {
				result = err
			}
		}
		ch <- 0
	}

loop:
	for {
		result = nil
		c.EE.Once("Login", listener)
		c.EE.Once("LoginError", listener)
		c.EE.Once("Closed", listener)

		select {
		case <-ch:

		case <-ctx.Done():
			if result == nil {
				result = ctx.Err()
			}
			break loop
		}

		c.EE.RemoveListener("Login", listener)
		c.EE.RemoveListener("LoginError", listener)
		c.EE.RemoveListener("Closed", listener)

		if result == nil {
			result = c.whenReady()
			if result == errorNotReady {
				continue
			}
		}
		break
	}
	return result
}

// NextRequestID returns the next request id
func (c *Client) NextRequestID() uint64 {
	requestID := atomic.AddUint64(&c.nextRequestID, uint64(1))
	return requestID
}

// MakeLoginMessage makes a login request
func (c *Client) MakeLoginMessage() *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestID(),
		Payload: &protocol.Message_Request{
			Request: &protocol.Request{
				Payload: &protocol.Request_Login{
					Login: &protocol.LoginRequest{
						AccessToken: c.AccessToken,
						ClientKey:   c.ClientKey,
					},
				},
			},
		},
	}
	return &msg
}

// MakeErrorMessage makes an error response
func (c *Client) MakeErrorMessage(request *protocol.Message, errorCode protocol.ErrorCode, errorMessage string) *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_RESPONSE,
		RequestId:   request.RequestId,
		Payload: &protocol.Message_Response{
			Response: &protocol.Response{
				Payload: &protocol.Response_Error{
					Error: &protocol.ErrorResponse{
						Code:    uint32(errorCode),
						Message: errorMessage,
					},
				},
			},
		},
	}

	return &msg
}

// MakePingResponseMessage makes a ping response
func (c *Client) MakePingResponseMessage(request *protocol.Message) *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_RESPONSE,
		RequestId:   request.RequestId,
		Payload: &protocol.Message_Response{
			Response: &protocol.Response{
				Payload: &protocol.Response_Ping{
					Ping: &protocol.PingResponse{
						Time: request.GetRequest().GetPing().Time,
					},
				},
			},
		},
	}
	return &msg
}

// MakeCreateTaskMessage makes a create request
func (c *Client) MakeCreateTaskMessage(req *CreateTaskRequest, options ITaskOptions) *protocol.Message {
	request := &protocol.CreateTaskRequest{
		ChallengeType: req.ChallengeType,
		TimeOut:       int32(req.Timeout),
		Url:           req.URL,
	}
	if options != nil {
		data, _ := json.Marshal(options)
		if len(data) > 0 {
			request.Options = string(data)
		}
	}
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestID(),
		Payload: &protocol.Message_Request{
			Request: &protocol.Request{
				Payload: &protocol.Request_CreateTask{
					CreateTask: request,
				},
			},
		},
	}

	return &msg
}

// MakeGetTaskResultMessage makes a get request
func (c *Client) MakeGetTaskResultMessage(req *GetTaskResultRequest) *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestID(),
		Payload: &protocol.Message_Request{
			Request: &protocol.Request{
				Payload: &protocol.Request_GetTaskResult{
					GetTaskResult: &protocol.GetTaskResultRequest{
						TaskId: req.TaskID,
					},
				},
			},
		},
	}
	return &msg
}

// MakeCancelTaskMessage makes a cancel request
func (c *Client) MakeCancelTaskMessage(req *CancelTaskRequest) *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestID(),
		Payload: &protocol.Message_Request{
			Request: &protocol.Request{
				Payload: &protocol.Request_CancelTask{
					CancelTask: &protocol.CancelTaskRequest{
						ChallengeType: req.ChallengeType,
						TaskId:        req.TaskID,
					},
				},
			},
		},
	}
	return &msg
}

// SendMessage sends a message to server
func (c *Client) SendMessage(msg *protocol.Message) error {
	data, err := proto.Marshal(msg)
	if err == nil {
		err = c.ws.SendBinary(data)
	}
	return err
}

// Invoke sends the request to server and blocks the current routine until corresponding response is received
func (c *Client) Invoke(context context.Context, message *protocol.Message) (*protocol.Message, error) {
	if !c.IsConnected() {
		return nil, ErrorNotConnected
	}

	if message.RequestId == 0 {
		message.RequestId = c.NextRequestID()
	}

	requestID := message.RequestId
	msg, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}

	responseChan := make(chan response)
	c.requests.Store(requestID, responseChan)

	listener := func() {
		responseChan <- response{
			Error: ErrorAborted,
		}
	}
	c.ee.Once("Abort", listener)

	defer (func() {
		c.requests.Delete(requestID)
		c.ee.Off("Abort", listener)
	})()
	err = c.ws.SendBinary(msg)
	if err != nil {
		return nil, err
	}

	select {
	case <-context.Done():
		return nil, context.Err()

	case response := <-responseChan:
		return response.Message, response.Error
	}
}

func (c *Client) addEventListeners() {
	c.ws.OnConnected = func(_ *gowebsocket.Socket) {
		go (func() {
			if !c.IsStarted() {
				return
			}
			msg := c.MakeLoginMessage()
			response, err := c.Invoke(context.Background(), msg)
			if err == nil {
				login := response.GetResponse().GetLogin()
				if login.Success {
					c.onLoggedIn()
				} else {
					c.onLoginError()
				}
			} else {
				c.onLoginError()
			}
		})()
	}
	c.ws.OnDisconnected = func(err error, _ *gowebsocket.Socket) {
		if c.IsStarted() {
			c.EE.Emit("Disconnected", err)

			go (func() {
				c.mutex.Lock()
				defer c.mutex.Unlock()

				if c.started {
					c._close()
					c.connect()
				}
			})()
		}
	}
	c.ws.OnConnectError = func(err error, _ *gowebsocket.Socket) {
		if c.IsStarted() {
			c.EE.Emit("ConnectError", err)

			go (func() {
				started := false
				func() {
					c.mutex.Lock()
					defer c.mutex.Unlock()

					if c.started {
						started = true
						c.retryCount++
						c._close()
					}
				}()

				if started {
					sleepTime := retryIntervals[c.retryCount%len(retryIntervals)]
					time.Sleep(time.Duration(sleepTime) * time.Millisecond)

					c.mutex.Lock()
					defer c.mutex.Unlock()

					if c.started {
						c.connect()
					}
				}
			})()
		}
	}

	c.ws.OnBinaryMessage = func(data []byte, _ *gowebsocket.Socket) {
		msg := protocol.Message{}
		proto.Unmarshal(data, &msg)

		if msg.GetNotification() != nil {
			go c.onNotification(&msg)
		} else if msg.GetResponse() != nil {
			c.onResponse(&msg)
		} else if msg.GetRequest() != nil {
			go c.onRequest(&msg)
		}
	}
}

func (c *Client) removeEventListeners() {
	c.ws.OnConnected = nil
	c.ws.OnConnectError = nil
	c.ws.OnDisconnected = nil
	c.ws.OnBinaryMessage = nil
}

func (c *Client) onNotification(msg *protocol.Message) {
	evt := NotificationEvent{
		Message:      msg,
		Notification: msg.GetNotification(),
		Handled:      0,
	}

	if evt.Notification == nil {
		return
	}
	typeName := reflect.TypeOf(evt.Notification.Payload).String()
	notificationName := "Notification-" + typeName

	c.EE.Emit(notificationName, &evt)
	if evt.Handled == 0 {
		c.EE.Emit("Notification", &evt)
	}
}

func (c *Client) onResponse(msg *protocol.Message) {
	if responseChan, ok := c.requests.Load(msg.RequestId); ok {

		if ch, ok := responseChan.(chan response); ok {
			responseMsg := msg
			var err error

			if msg.GetResponse().GetError() != nil {
				responseError := msg.GetResponse().GetError()
				responseMsg = nil
				err = NewRemoteError(int(responseError.Code), responseError.Message)
			}
			ch <- response{Message: responseMsg, Error: err}
		}
	}
}

func (c *Client) onRequest(requestMsg *protocol.Message) {
	if requestMsg.GetRequest() != nil {
		switch requestMsg.GetRequest().Payload.(type) {
		case *protocol.Request_Ping:
			c.heartBeatCount = 0
			msg := c.MakePingResponseMessage(requestMsg)
			c.SendMessage(msg)

		default:
			msg := c.MakeErrorMessage(requestMsg, protocol.ErrorCode_INVALID_REQUEST, "unsupported request")
			c.SendMessage(msg)
		}

	}
}

func (c *Client) onLoggedIn() {

	started := false
	(func() {
		// Takes the c.mutex first
		c.mutex.Lock()
		defer c.mutex.Unlock()

		if c.started {
			started = true
			c.loggedIn = true

			c.connMutex.Lock()
			defer c.connMutex.Unlock()

			c.startHeartBeatCheck()
		}
	})()

	// Don't emit the login event if this client is already stopped
	if started {
		c.EE.Emit("Login")
	}
}

func (c *Client) onLoginError() {
	started := false
	(func() {
		c.mutex.Lock()
		defer c.mutex.Unlock()

		if c.started {
			started = true
			c.loginError = true
		}
	})()

	// Don't emit the login event if this client is already stopped
	if started {
		c.EE.Emit("LoginError", ErrorLogin)
	}
}

func (c *Client) connect() error {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()

	if c.AccessToken == "" || c.ClientKey == "" {
		return ErrorNoAuthorizationInfo
	}
	if c.ws.IsConnected {
		return nil
	}

	c.loginError = false
	c.loggedIn = false

	c.addEventListeners()
	c.ws.IsConnected = false

	ctx, cancel := context.WithCancel(context.Background())

	c.cancelFunc = cancel
	go c.ws.ConnectWithContext(ctx)
	return nil
}

func (c *Client) close() {
	(func() {
		c.connMutex.Lock()
		defer c.connMutex.Unlock()

		c.removeEventListeners()

		if c.cancelFunc != nil {
			c.cancelFunc()
			c.cancelFunc = nil
		}
		c.ws.Close()

		c.stopHeartBeatCheck()
	})()

	c.ee.Emit("Abort")
}

func (c *Client) _close() {
	(func() {
		c.connMutex.Lock()
		defer c.connMutex.Unlock()

		c.removeEventListeners()
		if c.cancelFunc != nil {
			c.cancelFunc()
			c.cancelFunc = nil
		}

		c.stopHeartBeatCheck()
	})()

	c.ee.Emit("Abort")
}

func (c *Client) startHeartBeatCheck() {
	c.ticker = time.NewTicker(10 * time.Second)
	c.tickerChan = make(chan byte, 1)
	go func() {
		for {
			select {
			case <-c.tickerChan:
				return

			case <-c.ticker.C:
				c.heartBeatCount++
				if c.heartBeatCount > c.HeartBeatTimeout {
					c.close()
					c.connect()
					return
				}
			}
		}
	}()
}

func (c *Client) stopHeartBeatCheck() {
	if c.ticker == nil {
		return
	}
	c.tickerChan <- 0
	c.ticker.Stop()
	c.ticker = nil
}
