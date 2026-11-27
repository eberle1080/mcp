package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/authorization"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/stretchr/testify/require"
)

func TestDefaultTaskOwnerIgnoresHowTheTokenArrived(t *testing.T) {
	owner := func(token string) string {
		return defaultTaskOwner(context.WithValue(context.Background(), authorization.TokenKey, &authorization.Token{Token: token}))
	}
	require.Equal(t, owner("abc"), owner("Bearer abc"), "header and _meta tokens are the same caller")
	require.Equal(t, owner("abc"), owner("bearer  abc "))
	require.NotEqual(t, owner("abc"), owner("abd"))
	require.Equal(t, "", defaultTaskOwner(context.Background()))
	require.Equal(t, "", owner("Bearer "))
}

func TestTaskNotificationsKeepNumbersExact(t *testing.T) {
	notification, err := taskNotification(7, tasks.Task{TaskID: "t", Status: tasks.Completed, CreatedAt: "t", LastUpdatedAt: "t",
		Result: json.RawMessage(`{"content":[],"structuredContent":{"id":9007199254740993,"ratio":0.1}}`)})
	require.NoError(t, err)
	require.True(t, strings.Contains(string(notification.Params), `"id":9007199254740993`), string(notification.Params))
	require.Contains(t, string(notification.Params), `"io.modelcontextprotocol/subscriptionId":7`)
}

func TestTaskStreamKeepsOnlyTheLatestStatePerTask(t *testing.T) {
	var sent []string
	stream := newTaskStream(context.Background(), func(_ context.Context, task tasks.Task) {
		sent = append(sent, task.TaskID+":"+string(task.Status))
	})
	for i := 0; i < 1000; i++ {
		stream.enqueue(tasks.Task{TaskID: "a", Status: tasks.Working})
	}
	stream.enqueue(tasks.Task{TaskID: "b", Status: tasks.Working})
	stream.enqueue(tasks.Task{TaskID: "a", Status: tasks.Completed})
	stream.mu.Lock()
	require.Len(t, stream.order, 2, "a slow reader does not make the queue grow")
	stream.mu.Unlock()
	stream.release()
	stream.stop()
	require.Equal(t, []string{"a:completed", "b:working"}, sent)
}

func TestListenSendsCurrentStatesBeforeChanges(t *testing.T) {
	m := newTaskManager()
	m.owner = func(context.Context) string { return "alice" }
	release := make(chan struct{})
	task, err := m.start(context.Background(), func(ctx context.Context, _ tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
		<-release
		return nil, nil
	}, tasks.StartOptions{})
	require.NoError(t, err)
	var seen []tasks.Status
	accepted, stop := m.listen(context.Background(), []string{task.TaskID, "missing"}, func(task tasks.Task) { seen = append(seen, task.Status) })
	defer stop()
	require.Equal(t, []string{task.TaskID}, accepted)
	require.Equal(t, []tasks.Status{tasks.Working}, seen, "the current state is handed over under the lock, first")

	_, stopNone := m.listen(context.Background(), []string{"missing"}, func(tasks.Task) {})
	stopNone()
	m.mu.Lock()
	require.Len(t, m.listeners, 1, "a stream that follows nothing is not registered")
	m.mu.Unlock()
	close(release)
}
