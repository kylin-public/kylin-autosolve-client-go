package main

import (
	"context"
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

	results sync.Map
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

func (s *AutosolveService) Solve(ctx context.Context, req *as.CreateTaskRequest, options as.ITaskOptions) (*protocol.TaskResultNotification, error) {
	err := s.Client.WhenReadyWithContext(ctx)
	if err != nil {
		return nil, err
	}
	response, err := s.Client.Invoke(ctx, s.Client.MakeCreateTaskMessage(req, options))
	if err != nil {
		return nil, err
	}

	taskID := response.GetResponse().GetCreateTask().TaskId
	ch := make(chan *protocol.TaskResultNotification, 1)
	s.results.Store(taskID, ch)

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
	flag.Parse()

	if accessToken == nil || *accessToken == "" {
		fmt.Println("Invalid access token:", accessToken)
		return
	}

	service := AutosolveService{
		AccessToken: *accessToken,
	}

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
		}
	}

	for i := 0; i < 3; i++ {
		go demo()
	}

	for i := 0; i < 3; i++ {
		<-doneCh
	}

	service.Stop()
}
