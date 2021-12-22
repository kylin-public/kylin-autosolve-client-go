package main

import (
	"context"
	"flag"
	"fmt"

	as "github.com/kylin-public/kylin-autosolve-client-go"
)

var clientKey = "w8mp5inwszowft3kyc"

func main() {
	accessToken := flag.String("token", "", "the access token")
	flag.Parse()

	if accessToken == nil || *accessToken == "" {
		fmt.Println("Invalid access token:", accessToken)
		return
	}

	client := as.New()
	client.AccessToken = *accessToken
	client.ClientKey = clientKey

	client.Start()
	if client.WhenReady() == nil {
		response, err := client.Invoke(context.Background(), client.MakeCreateTaskMessage(&as.CreateTaskRequest{
			ChallengeType: "google",
			URL:           "https://recaptcha-test.kylinbot.io/",
		}, &as.TaskOptions{
			SiteKey: "6Lfv-q0ZAAAAADy0U9JUaCPCZI15U-7jhbAiYa0U",
			Version: "3",
			Action:  "login",
		}))
		if err != nil {
			fmt.Println("could not create task:", err.Error())
		} else {
			fmt.Println("response:", response)

			result, err := client.Invoke(context.Background(), client.MakeCancelTaskMessage(&as.CancelTaskRequest{
				TaskID: response.GetResponse().GetCreateTask().TaskId,
			}))
			if err != nil {
				fmt.Println("could not cancel the task:", err.Error())
			} else {
				fmt.Println("cancel result:", result)
			}
		}
	}
	client.Stop()
}
