package kube

import (
	"io"
	"time"
)

type WatchFeed interface {
	WriteJobLogChunk(*JobLogChunk) error
}

type JobLogLine struct {
	Timestamp string
	Data      string
}

type JobLogChunk struct {
	Name          string
	PodName       string
	ContainerName string
	LogLines      []JobLogLine
}

type WriteJobLogChunkFunc func(*JobLogChunk) error

func (f WriteJobLogChunkFunc) WriteJobLogChunk(chunk *JobLogChunk) error {
	return f(chunk)
}

func (c *Client) WatchJobTillDone(namespace string, jobName string, reader io.Reader, watchFeed WatchFeed, timeout time.Duration) error {
	for i := 0; i < 10; i += 1 {
		jfi := &JobLogChunk{
			Name:          "my-job",
			PodName:       "my-pod",
			ContainerName: "my-container",
			LogLines: []JobLogLine{
				JobLogLine{Timestamp: "Mon Feb 12 17:17:42 MSK 2018", Data: "Hello World"},
			},
		}
		watchFeed.WriteJobLogChunk(jfi)
	}

	return nil
}
