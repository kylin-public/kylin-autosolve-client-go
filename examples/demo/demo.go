package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"sync"

	as "github.com/kylin-public/kylin-autosolve-client-go"
	"github.com/kylin-public/kylin-autosolve-client-go/protocol"
)

var clientKey = "w8mp5inwszowft3kyc"

type AutosolveService struct {
	Client      *as.Client
	AccessToken string

	results        sync.Map
	mutex          sync.Mutex
	buffered       map[string]*protocol.TaskResultNotification
	bufferingCount int
}

func NewAutosolveService(accessToken string) *AutosolveService {
	return &AutosolveService{
		AccessToken: accessToken,
		buffered:    make(map[string]*protocol.TaskResultNotification),
	}
}

func (s *AutosolveService) Initialize() {
	if s.Client == nil {
		s.Client = as.New()
		s.Client.ClientKey = clientKey

		s.Client.EE.On(as.NotificationTaskResult, func(payload ...interface{}) {
			if len(payload) > 0 {
				if evt, ok := payload[0].(*as.NotificationEvent); ok {

					taskResult := evt.Notification.GetTaskResult()
					if taskResult != nil {
						if value, ok := s.results.LoadAndDelete(taskResult.TaskId); ok {
							if ch, ok := value.(chan *protocol.TaskResultNotification); ok {
								ch <- taskResult
							}
						} else {
							s.bufferResult(taskResult.TaskId, taskResult)
						}
					}
				}
			}
		})
	}
}

func (s *AutosolveService) Start() {
	if s.Client == nil {
		s.Initialize()
	}
	s.Client.AccessToken = s.AccessToken
	s.Client.Start()
}

func (s *AutosolveService) enableBuffering() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.bufferingCount++
}

func (s *AutosolveService) disableBuffering() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.bufferingCount > 0 {
		s.bufferingCount--
		if s.bufferingCount == 0 {
			for k := range s.buffered {
				delete(s.buffered, k)
			}
		}
	}
}

func (s *AutosolveService) bufferResult(taskID string, result *protocol.TaskResultNotification) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.bufferingCount > 0 {
		s.buffered[taskID] = result
	}
}

func (s *AutosolveService) Solve(ctx context.Context, req *as.CreateTaskRequest, options as.ITaskOptions) (*protocol.TaskResultNotification, error) {
	err := s.Client.WhenReadyWithContext(ctx)
	if err != nil {
		return nil, err
	}

	ch := make(chan *protocol.TaskResultNotification, 1)
	response, result, err := func() (*protocol.Message, *protocol.TaskResultNotification, error) {
		s.enableBuffering()
		defer s.disableBuffering()

		response, err := s.Client.Invoke(ctx, s.Client.MakeCreateTaskMessage(req, options))
		if err != nil {
			return response, nil, err
		}

		taskID := response.GetResponse().GetCreateTask().TaskId

		s.mutex.Lock()

		buffered := s.buffered[taskID]
		if buffered != nil {
			delete(s.buffered, taskID)
		} else {
			s.results.Store(taskID, ch)
		}

		s.mutex.Unlock()

		return response, buffered, err
	}()
	if err != nil {
		return nil, err
	}

	if result != nil {
		return result, nil
	}

	taskID := response.GetResponse().GetCreateTask().TaskId

	select {
	case result := <-ch:
		return result, nil

	case <-ctx.Done():
		s.results.Delete(taskID)
		return nil, ctx.Err()
	}
}

func (s *AutosolveService) Stop() {
	if s.Client != nil {
		if s.Client.IsLoggedIn() {
			s.Client.Invoke(context.Background(), s.Client.MakeCancelTaskMessage(&as.CancelTaskRequest{}))
		}
		s.Client.Stop()
	}
}

func main() {
	accessToken := flag.String("token", "", "the access token")
	n := flag.Int("n", 1, "the number of challenges to solve")
	flag.Parse()

	if accessToken == nil || *accessToken == "" {
		fmt.Println("Invalid access token:", accessToken)
		return
	}

	service := NewAutosolveService(*accessToken)

	service.Start()

	doneCh := make(chan byte, 3)

	demo := func() {
		defer (func() {
			doneCh <- 0
		})()

		result, err := service.Solve(context.Background(), &as.CreateTaskRequest{
			ChallengeType: "google",
			URL:           "https://recaptcha-test.kylinbot.io/",
		}, &as.TaskOptions{
			SiteKey: "6Lfv-q0ZAAAAADy0U9JUaCPCZI15U-7jhbAiYa0U",
			Version: "3",
			Action:  "login",
		})
		if err != nil {
			fmt.Println("could not create task:", err.Error())
		} else {
			fmt.Println("task result:", result)

			if result != nil && result.Token != "" {
				var token interface{}
				if json.Unmarshal([]byte(result.Token), &token) == nil {
					fmt.Println("token:", token)
				}
			}
		}
	}

	for i := 0; i < *n; i++ {
		go demo()
	}

	for i := 0; i < *n; i++ {
		<-doneCh
	}

	service.Stop()
}
