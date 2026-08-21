package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/mashiike/lambroutines"
)

func handle(ctx context.Context, payload json.RawMessage) (string, error) {
	log.Printf("[handler] invoked")
	err := lambroutines.Go(ctx, func(context.Context) error {
		start := time.Now()
		log.Printf("[bg] background task started")
		time.Sleep(3 * time.Second)
		log.Printf("[bg] background task done after %s", time.Since(start))
		return nil
	})
	if err != nil {
		return "", err
	}
	log.Printf("[handler] doing more work after Go()")
	time.Sleep(1 * time.Second)
	log.Printf("[handler] returning response")
	return "ok", nil
}

func main() {
	lch, err := lambroutines.Start()
	if err != nil {
		log.Fatalf("lambroutines: start failed: %v", err)
	}
	lambda.Start(lch.Wrap(handle))
}
