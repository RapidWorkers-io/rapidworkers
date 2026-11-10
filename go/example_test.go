package rapidworkers_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	rapidworkers "github.com/RapidWorkers-io/rapidworkers/go"
)

// Quote an AI data-collection task, create it once the cost is approved, then
// review the work. (Examples without an Output comment are compiled but not
// run, so this never calls the API.)
func Example() {
	ctx := context.Background()
	rw, err := rapidworkers.NewClientFromEnv() // reads RAPIDWORKERS_API_KEY
	if err != nil {
		log.Fatal(err)
	}

	// 1. Get a quote (nothing is charged).
	quote, err := rw.PreviewJobCost(ctx, rapidworkers.PreviewJobCostParams{
		Kind:     rapidworkers.KindAITask,
		PayRate:  0.25,
		Quantity: 100,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(quote["total"], quote["expires_at"]) // show the total and get a yes first

	// 2. Create the task with that quote_id once the cost is approved.
	task, err := rw.CreateAITask(ctx, rapidworkers.CreateAITaskParams{
		TaskType:               rapidworkers.AITaskTypeImageCollection,
		Title:                  "Photo of a paper receipt",
		Description:            "Take a clear photo of one paper receipt from a recent purchase.",
		SubmissionInstructions: "Upload one photo. The whole receipt must be visible and readable.",
		RequiredFileCount:      rapidworkers.Int(1),
		PayRate:                0.25,
		Quantity:               100,
		LengthInMinutes:        5,
		Countries:              []string{"All"},
		QuoteID:                quote["quote_id"].(string),
		Confirm:                true,
	})
	if err != nil {
		log.Fatal(err)
	}
	jobID := task["job_id"].(string)

	// 3. Review the work.
	subs, err := rw.ListSubmissions(ctx, rapidworkers.ListSubmissionsParams{
		JobID:  jobID,
		Kind:   rapidworkers.KindAITask,
		Status: rapidworkers.SubmissionStatusPending,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range subs["submissions"].([]any) {
		sub := s.(map[string]any)
		_, err := rw.ApproveSubmission(ctx, rapidworkers.ApproveSubmissionParams{
			JobID:        jobID,
			Kind:         rapidworkers.KindAITask,
			SubmissionID: int(sub["submission_id"].(float64)),
		})
		if err != nil {
			log.Fatal(err)
		}
	}
}

func ExampleToolError() {
	ctx := context.Background()
	rw, err := rapidworkers.NewClient("your-api-key")
	if err != nil {
		log.Fatal(err)
	}
	_, err = rw.CreateJob(ctx, rapidworkers.CreateJobParams{ /* ... */ })
	var toolErr *rapidworkers.ToolError
	if errors.As(err, &toolErr) {
		// The tool refused the call; Message says how to fix it.
		fmt.Println(toolErr.Code, toolErr.Message)
	}
}

func ExampleClient_CallTool() {
	ctx := context.Background()
	rw, err := rapidworkers.NewClientFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	info, err := rw.CallTool(ctx, "get_platform_info", map[string]any{"section": "limits"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(info)
}
