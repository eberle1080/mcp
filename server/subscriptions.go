package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
)

type subscriptionFilterer interface {
	FilterSubscriptions(context.Context, schema.SubscriptionFilter) schema.SubscriptionFilter
}

// Listen establishes the July notification stream, acknowledges the accepted
// filter first, and remains active until its request context is cancelled.
func (h *Handler) Listen(ctx context.Context, request *jsonrpc.Request) (*schema.SubscriptionsListenResult, *jsonrpc.Error) {
	var params schema.SubscriptionsListenRequestParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return nil, jsonrpc.NewInvalidParamsError(fmt.Sprintf("failed to parse: %v", err), request.Params)
	}
	id, ok := jsonrpc.AsRequestIntId(request.Id)
	if !ok || id <= 0 {
		return nil, jsonrpc.NewInvalidRequest("subscriptions/listen requires a positive integer request id", request.Params)
	}
	subscriptionID := schema.RequestId(id)
	// Task notifications (the Tasks extension) need the client's declaration.
	taskIDs, err := requestedTaskIDs(request.Params)
	if err != nil {
		return nil, jsonrpc.NewInvalidParamsError(fmt.Sprintf("failed to parse: %v", err), request.Params)
	}
	if h.Server == nil || h.tasks == nil {
		taskIDs = nil // a server without tasks ignores the field, as it always has
	}
	if len(taskIDs) > 0 && !requestDeclaresTasks(ctx) {
		return nil, tasks.MissingCapabilityError()
	}
	accepted := schema.SubscriptionFilter{}
	if filterer, ok := h.handler.(subscriptionFilterer); ok {
		accepted = filterer.FilterSubscriptions(ctx, params.Notifications)
	}
	if h.Notifier == nil {
		return nil, jsonrpc.NewInternalError("subscriptions/listen requires a notification-capable transport", nil)
	}
	var acceptedTasks []string
	var stream *taskStream
	if len(taskIDs) > 0 {
		notifier := h.Notifier
		stream = newTaskStream(ctx, func(ctx context.Context, task tasks.Task) {
			if notification, err := taskNotification(subscriptionID, task); err == nil {
				_ = notifier.Notify(ctx, notification)
			}
		})
		defer stream.stop()
		// Each accepted task's current state is queued first, so a task that
		// finished before the stream opened is not waited on forever. The
		// stream sends nothing until the acknowledgement is out.
		var stopTasks func()
		acceptedTasks, stopTasks = h.tasks.listen(ctx, taskIDs, stream.enqueue)
		defer stopTasks()
	}
	raw, err := json.Marshal(schema.SubscriptionsAcknowledgedNotificationParams{
		Meta:          &schema.NotificationMetaObject{IoModelcontextprotocolSubscriptionId: &subscriptionID},
		Notifications: accepted,
	})
	if err == nil && len(acceptedTasks) > 0 {
		raw, err = withAcknowledgedTaskIDs(raw, acceptedTasks)
	}
	if err != nil {
		return nil, jsonrpc.NewInternalError(err.Error(), nil)
	}
	ack := &jsonrpc.Notification{Jsonrpc: jsonrpc.Version, Method: schema.MethodNotificationSubscriptionsAcknowledged, Params: raw}
	if h.subscriptions != nil {
		stop, err := h.subscriptions.open(ctx, subscriptionID, accepted, ack)
		if err != nil {
			return nil, jsonrpc.NewInternalError(err.Error(), nil)
		}
		defer stop()
	} else if err = h.Notifier.Notify(ctx, ack); err != nil {
		return nil, jsonrpc.NewInternalError(err.Error(), nil)
	}
	if stream != nil {
		stream.release()
	}

	<-ctx.Done()
	info := h.info
	return &schema.SubscriptionsListenResult{
		Meta: schema.SubscriptionsListenResultMetaObject{
			IoModelcontextprotocolServerInfo:     &info,
			IoModelcontextprotocolSubscriptionId: subscriptionID,
		},
		ResultType: completeResultType,
	}, nil
}

// withAcknowledgedTaskIDs adds the accepted task IDs to an acknowledgement's
// notifications object, which the core schema does not model.
func withAcknowledgedTaskIDs(raw json.RawMessage, ids []string) (json.RawMessage, error) {
	var params map[string]interface{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	notifications, _ := params["notifications"].(map[string]interface{})
	if notifications == nil {
		notifications = map[string]interface{}{}
	}
	notifications["taskIds"] = ids
	params["notifications"] = notifications
	return json.Marshal(params)
}
