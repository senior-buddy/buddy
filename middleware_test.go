package buddy

import (
	"testing"
)

var (
	Middleware = NewMiddlewarePipeline()
)

func TestMiddlewareRun(t *testing.T) {
	go Middleware.Run()
}
