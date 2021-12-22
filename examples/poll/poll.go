package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	as "github.com/kylin-public/kylin-autosolve-client-go"
)

var kClientKey = "w8mp5inwszowft3kyc"

func main() {
	accessToken := flag.String("token", "", "the access token")
	flag.Parse()

	if accessToken == nil || *accessToken == "" {
		fmt.Println("Invalid access token:", accessToken)
		return
	}

	client := as.NewAutosolveClient()
	client.AccessToken = *accessToken
	client.ClientKey = kClientKey

	client.Start()
	if client.WhenReady() == nil {
		response, err := client.Invoke(context.Background(), client.MakeCreateTaskMessage(&as.CreateTaskRequest{
			ChallengeType: "google",
			Url:           "https://recaptcha-test.kylinbot.io/",
		}, &as.TaskOptions{
			SiteKey: "6Lfv-q0ZAAAAADy0U9JUaCPCZI15U-7jhbAiYa0U",
			Version: "3",
			Action:  "login",
		}))
		if err != nil {
			fmt.Println("could not create task:", err.Error())
		} else {
			fmt.Println("response:", response)

			for i := 0; i < 10; i++ {
				result, err := client.Invoke(context.Background(), client.MakeGetTaskResultMessage(&as.GetTaskResultRequest{
					TaskId: response.GetResponse().GetCreateTask().TaskId,
				}))
				if err != nil {
					fmt.Println("could not get task result:", err.Error())
					break
				} else {
					fmt.Println("task result:", result)
					if result.GetResponse().GetGetTaskResult().ErrorId != 1 {
						break
					}
				}

				time.Sleep(time.Second)
			}
		}
	}
	client.Stop()
}
