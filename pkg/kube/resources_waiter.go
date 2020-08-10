package kube

import (
	"context"
	"time"
)

type ResourcesWaiter interface {
	Wait(ctx context.Context, namespace string, resources ResourceList, timeout time.Duration) error
	WatchUntilReady(ctx context.Context, namespace string, resources ResourceList, timeout time.Duration) error
}
