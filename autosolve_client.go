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

var kAutosolveUrl = "wss://autosolve-ws.kylinbot.io/ws"
var kRetryIntervals = []int{1000, 2000, 4000, 8000, 16000}

var NotificationTaskResult = "Notification-" + reflect.TypeOf(&protocol.Notification_TaskResult{}).String()

var ErrorAborted = errors.New("THE REQUEST IS ABORTED")

type RemoteError struct {
	s    string
	Code int
}

func NewRemoteError(code int, text string) error {
	return &RemoteError{text, code}
}

func (e *RemoteError) Error() string {
	return e.s
}

type AutosolveClient struct {
	ws gowebsocket.Socket

	AccessToken      string
	ClientKey        string
	HeartBeatTimeout int

	EE            ee.IEventEmitter
	nextRequestId uint64
	requests      sync.Map

	cancelFunc     context.CancelFunc
	retryCount     int
	loginError     bool
	ticker         *time.Ticker
	tickerChan     chan byte
	heartBeatCount int
	started        bool
	loggedIn       bool
}

type NotificationEvent struct {
	Message      *protocol.Message
	Notification *protocol.Notification
	Handled      int
}

type Param struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ITaskOptions interface{}

type InputInfo struct {
	Id         string `json:"id"`
	Timestamp  int64  `json:"timestamp"`
	InputLabel string `json:"input_label"`
}

type TaskOptions struct {
	ITaskOptions `json:"-"`

	Type               string     `json:"type,omitempty"`
	SiteKey            string     `json:"site_key,omitempty"`
	Version            string     `json:"version,omitempty"`
	Enterprise         *bool      `json:"enterprise,omitempty"`
	Invisible          *bool      `json:"invisible,omitempty"`
	Action             string     `json:"action,omitempty"`
	Secret             string     `json:"secret,omitempty"`
	Challenge          string     `json:"challenge,omitempty"`
	RqData             string     `json:"rqdata,omitempty"`
	ApiServer          string     `json:"api_server,omitempty"`
	ActionUrl          string     `json:"action_url,omitempty"`
	Method             string     `json:"method,omitempty"`
	Params             *[]Param   `json:"params,omitempty"`
	Document           string     `json:"document,omitempty"`
	CallbackUrlPattern string     `json:"callback_url_pattern,omitempty"`
	Filters            []string   `json:"filters,omitempty"`
	Info               *InputInfo `json:"info,omitempty"`
}

type CreateTaskRequest struct {
	ChallengeType string
	Url           string
	Timeout       int
}

type GetTaskResultRequest struct {
	TaskId string
}

type CancelTaskRequest struct {
	ChallengeType string
	TaskId        string
}

type response struct {
	Message *protocol.Message
	Error   error
}

func NewAutosolveClient() *AutosolveClient {
	c := &AutosolveClient{
		ws:               gowebsocket.New(kAutosolveUrl),
		EE:               ee.NewEventEmitter(),
		HeartBeatTimeout: 8,
		nextRequestId:    1000,
	}

	return c
}

func (c *AutosolveClient) IsConnected() bool {
	return c.ws.IsConnected
}

func (c *AutosolveClient) IsLoggedIn() bool {
	return c.loggedIn
}

func (c *AutosolveClient) IsStarted() bool {
	return c.started
}

func (c *AutosolveClient) Start() error {
	if c.started {
		return nil
	}

	if err := c.connect(); err != nil {
		return err
	}
	c.started = true
	return nil
}

func (c *AutosolveClient) Stop() {
	if c.started {
		c.started = false
		c.close()

		c.EE.Emit("Closed")
	}
}

func (c *AutosolveClient) WhenReady() error {
	if c.started && c.loggedIn {
		return nil
	}
	if !c.started {
		return errors.New("NOT STARTED")
	}
	if c.loginError {
		return errors.New("NOT LOGGED IN")
	}

	var result error

	ch := make(chan byte)
	listener := func(payload ...interface{}) {
		if len(payload) > 0 {
			if err, ok := payload[0].(error); ok {
				result = err
			}
		}
		ch <- 0
	}
	c.EE.Once("Login", listener)
	c.EE.Once("LoginError", listener)
	c.EE.Once("Closed", listener)

	<-ch

	c.EE.RemoveListener("Login", listener)
	c.EE.RemoveListener("LoginError", listener)
	c.EE.RemoveListener("Closed", listener)

	if result == nil {
		if !c.started {
			return errors.New("ALREADY STOPPED")
		}
		if c.started && c.loggedIn {
			return nil
		}
	}
	return result
}

func (c *AutosolveClient) NextRequestId() uint64 {
	requestId := atomic.AddUint64(&c.nextRequestId, uint64(1))
	return requestId
}

func (c *AutosolveClient) MakeLoginMessage() *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestId(),
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

func (c *AutosolveClient) MakeErrorMessage(request *protocol.Message, errorCode protocol.ErrorCode, errorMessage string) *protocol.Message {
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

func (c *AutosolveClient) MakePingResponseMessage(request *protocol.Message) *protocol.Message {
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

func (c *AutosolveClient) MakeCreateTaskMessage(req *CreateTaskRequest, options ITaskOptions) *protocol.Message {
	request := &protocol.CreateTaskRequest{
		ChallengeType: req.ChallengeType,
		TimeOut:       int32(req.Timeout),
		Url:           req.Url,
	}
	if options != nil {
		data, _ := json.Marshal(options)
		if len(data) > 0 {
			request.Options = string(data)
		}
	}
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestId(),
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

func (c *AutosolveClient) MakeGetTaskResultMessage(req *GetTaskResultRequest) *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestId(),
		Payload: &protocol.Message_Request{
			Request: &protocol.Request{
				Payload: &protocol.Request_GetTaskResult{
					GetTaskResult: &protocol.GetTaskResultRequest{
						TaskId: req.TaskId,
					},
				},
			},
		},
	}
	return &msg
}

func (c *AutosolveClient) MakeCancelTaskMessage(req *CancelTaskRequest) *protocol.Message {
	msg := protocol.Message{
		MessageType: protocol.MessageType_REQUEST,
		RequestId:   c.NextRequestId(),
		Payload: &protocol.Message_Request{
			Request: &protocol.Request{
				Payload: &protocol.Request_CancelTask{
					CancelTask: &protocol.CancelTaskRequest{
						ChallengeType: req.ChallengeType,
						TaskId:        req.TaskId,
					},
				},
			},
		},
	}
	return &msg
}

func (c *AutosolveClient) SendMessage(msg *protocol.Message) error {
	data, err := proto.Marshal(msg)
	if err == nil {
		err = c.ws.SendBinary(data)
	}
	return err
}

func (c *AutosolveClient) Invoke(context context.Context, message *protocol.Message) (*protocol.Message, error) {
	if !c.IsConnected() {
		return nil, errors.New("NOT CONNECTED")
	}

	if message.RequestId == 0 {
		message.RequestId = c.NextRequestId()
	}

	requestId := message.RequestId
	msg, err := proto.Marshal(message)
	if err != nil {
		return nil, err
	}

	responseChan := make(chan response)
	c.requests.Store(requestId, responseChan)

	listener := func() {
		responseChan <- response{
			Error: ErrorAborted,
		}
	}
	c.EE.Once("Abort", listener)

	defer (func() {
		c.requests.Delete(requestId)
		c.EE.Off("Abort", listener)
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

func (c *AutosolveClient) addEventListeners() {
	c.ws.OnConnected = func(_ gowebsocket.Socket) {
		go (func() {
			if !c.started {
				return
			}
			msg := c.MakeLoginMessage()
			response, err := c.Invoke(context.Background(), msg)
			if err == nil {
				login := response.GetResponse().GetLogin()
				if login.Success {
					c.startHeartBeatCheck()
					c.loggedIn = true
					c.EE.Emit("Login")
				} else {
					c.loginError = true
					c.EE.Emit("LoginError", errors.New("COULD NOT LOGIN"))
				}
			} else {
				c.loginError = true
				c.EE.Emit("LoginError", err)
			}
		})()
	}
	c.ws.OnDisconnected = func(err error, _ gowebsocket.Socket) {
		c.EE.Emit("Disconnected", err)
		if c.started {
			go (func() {
				if c.started {
					c._close()

					c.connect()
				}
			})()
		}
	}
	c.ws.OnConnectError = func(err error, _ gowebsocket.Socket) {
		c.EE.Emit("ConnectError", err)
		if c.started {
			go (func() {
				if !c.started {
					return
				}
				c._close()

				c.retryCount++
				sleepTime := kRetryIntervals[c.retryCount]
				time.Sleep(time.Duration(sleepTime) * time.Millisecond)
				if c.started {
					c.connect()
				}
			})()
		}
	}

	c.ws.OnBinaryMessage = func(data []byte, _ gowebsocket.Socket) {
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

func (c *AutosolveClient) removeEventListeners() {
	c.ws.OnConnected = nil
	c.ws.OnConnectError = nil
	c.ws.OnDisconnected = nil
	c.ws.OnBinaryMessage = nil
}

func (c *AutosolveClient) onNotification(msg *protocol.Message) {
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

func (c *AutosolveClient) onResponse(msg *protocol.Message) {
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

func (c *AutosolveClient) onRequest(requestMsg *protocol.Message) {
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

func (c *AutosolveClient) connect() error {
	if c.AccessToken == "" || c.ClientKey == "" {
		return errors.New("INVALID ACCESS TOKEN OR CLIENT KEY")
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

func (c *AutosolveClient) close() {
	c.removeEventListeners()

	if c.cancelFunc != nil {
		c.cancelFunc()
		c.cancelFunc = nil
	}
	c.ws.Close()

	c.stopHeartBeatCheck()

	c.EE.Emit("Abort")
}

func (c *AutosolveClient) _close() {
	c.removeEventListeners()
	if c.cancelFunc != nil {
		c.cancelFunc()
		c.cancelFunc = nil
	}

	c.stopHeartBeatCheck()

	c.EE.Emit("Abort")
}

func (c *AutosolveClient) startHeartBeatCheck() {
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

func (c *AutosolveClient) stopHeartBeatCheck() {
	if c.ticker == nil {
		return
	}
	c.tickerChan <- 0
	c.ticker.Stop()
	c.ticker = nil
}
