# Tasks

The Tasks extension (`io.modelcontextprotocol/tasks`, MCP 2026-07-28) lets a server answer a `tools/call` with a task instead of a result. The client gets a task ID at once and comes back for the outcome:
- it polls `tasks/get`, or follows `notifications/tasks` on a `subscriptions/listen` stream;
- it answers the task's input requests with `tasks/update`;
- it can stop the task with `tasks/cancel`.

The client opts in on each request by declaring the extension, and the server decides per call whether to create a task. Tasks exist only in 2026-07-28. Older protocol versions get plain results.

The spec is [ext-tasks, specification/2026-07-28/tasks.md](https://github.com/modelcontextprotocol/ext-tasks/blob/main/specification/2026-07-28/tasks.md). [`example/tasks`](../example/tasks) shows both ends; run it with `go run ./example/tasks/cmd`.

## Serving tasks

Enable the runtime on the server:

```go
srv, err := server.New(
    server.WithNewHandler(newHandler),
    server.WithTasks(server.WithTaskTTL(time.Hour)),
)
```

This declares the extension in `server/discover` and serves `tasks/get`, `tasks/update` and `tasks/cancel`. Without `WithTasks` none of this exists, and tools always run in the call.

A tool answers with a task by calling `tasks.Start` from `github.com/eberle1080/mcp-protocol/extension/tasks`:

```go
func(ctx context.Context, input *ReportInput) (*schema.CallToolResult, *jsonrpc.Error) {
    work := func(ctx context.Context, task tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
        task.Progress("collecting figures")
        answer, err := task.Elicit(ctx, &schema.ElicitRequestParams{Message: "Include unaudited figures?" /* ... */})
        if err != nil {
            return nil, jsonrpc.NewInternalError(err.Error(), nil)
        }
        return buildReport(answer), nil
    }
    if _, err := tasks.Start(ctx, work); err == nil {
        return nil, nil // the client is sent the task; this return is ignored
    } else if !errors.Is(err, tasks.ErrUnavailable) {
        return nil, jsonrpc.NewInternalError(err.Error(), nil)
    }
    return buildReport(nil), nil // this request cannot take a task: do the work now
}
```

`Start` returns `tasks.ErrUnavailable` in any of these cases:
- the client did not declare the extension on this request;
- the request predates 2026-07-28;
- the server runs no tasks;
- the server already holds its maximum number of tasks.

The tool then does the work itself. If it cannot, it returns `tasks.MissingCapabilityError()`, which reaches the client as the spec's `-32021` JSON-RPC error. A server that converts tool errors into `isError` results leaves this one alone. `tasks.Available(ctx)` tells a tool in advance.

### The work

- **It runs after the call returns.** Its context keeps the request's values, such as credentials, and is cancelled when the client cancels the task or the task expires. A wait for input ends then too, even when the work waits on a context of its own.
- **What it returns becomes the task's outcome.** A `CallToolResult` completes the task, including one with `isError: true`. A JSON-RPC error fails it. A panic fails it with an internal error.
- **`task.Progress(message)`** sets the status message clients see.
- **`task.Elicit`, `task.CreateMessage` and `task.Input`** ask the client for input. In 2026-07-28 a server cannot send requests to the client, so these become the task's `inputRequests`: the task is `input_required` until the client answers with `tasks/update`, and the call returns the answer. Each request gets a key that is never reused.
- **Cancellation is cooperative.** After `tasks/cancel` the work's context is done, and the task ends `cancelled` when the work returns.

### Options and limits

| Option | Default | |
| --- | --- | --- |
| `WithTaskTTL` | 1 hour | How long after creation a task is kept. When it elapses, the task is discarded and its work stopped. |
| `WithTaskPollInterval` | 1 second | The poll interval suggested to clients. |
| `WithMaxTasks` | 10,000 | How many tasks the server keeps at once. At the cap, `Start` returns `ErrUnavailable`. |
| `WithMaxTasksPerCaller` | 1,000 | How many of those one caller may hold, so no caller can fill the store for everyone. |
| `WithTaskOwner` | digest of the bearer token | Who a task belongs to. See below. |

`tasks.WithTTL`, `tasks.WithPollInterval` and `tasks.WithStatusMessage` adjust a single task.

Tasks live in the server process's memory: they do not survive a restart. Each client sends `tasks/*` requests with the task ID in the `Mcp-Name` header, so a deployment with several instances can route them to the instance that holds the task. The server refuses a request whose header does not match its `taskId`.

### Who may see a task

- **Task IDs:** each has 128 random bits, so no one can guess or enumerate them.
- **Owner binding:** every task is bound to the caller that created it, and is "not found" for anyone else.
- **Default owner:** a digest of the request's bearer token. It is the same whether the token came in the `Authorization` header or in `_meta`, and it binds the task to that exact token, which suits most servers. A server whose tokens are refreshed while tasks run should identify callers by a stable subject with `WithTaskOwner`.
- **Under the auth middleware:** with a global policy, `tasks/*` and `subscriptions/listen` need credentials like any other request, and answer `401` with the usual challenge without them.
- **Without authentication:** every caller is the same one, and the unguessable ID is all that protects a task.

## Using tasks from a client

Declare the extension:

```go
cli := client.New("host", "1.0", transport, client.WithExtension(tasks.ExtensionID, nil))
// or, with the mcp.NewClient helper:
options.Extensions = map[string]map[string]interface{}{tasks.ExtensionID: {}}
```

After that, `CallTool` handles tasks for you. When the server answers with a task, `CallTool`:
- polls it, honouring the server's interval (kept between 50 ms and a minute);
- answers its input requests with the client's handler, the same `Elicit`, `CreateMessage` or `ListRoots` that handles direct requests, so they get the same trust and user-facing treatment;
- answers each request once;
- returns the final result.

A failed or cancelled task is a `*client.TaskError`. For a failed task, `errors.As(err, &rpcErr)` gets the JSON-RPC error. `CallTool` cancels the task when it stops waiting:
- the context ends;
- the task asks for something the handler does not implement;
- a poll fails three times in a row. A single failed poll is just retried.

The polls are requests of their own: a request ID set on the call is used for `tools/call` only.

For direct control:

```go
result, task, err := cli.CallToolTask(ctx, params) // either a result or a task
state, err := cli.GetTask(ctx, task.TaskID)
err = cli.UpdateTask(ctx, task.TaskID, map[string]json.RawMessage{key: answer})
err = cli.CancelTask(ctx, task.TaskID)
result, err = cli.AwaitTask(ctx, *task)            // what CallTool does
```

A client that did not declare the extension never gets a task. If a server sends one anyway, the call fails rather than misreading it.

### Notifications

Instead of polling, a client may name tasks in `subscriptions/listen`, as `notifications.taskIds`. This requires the declaration; without it the server answers `-32021`. The stream works as follows:
- **Acknowledgement:** it lists the tasks the server accepted, which are the caller's own tasks that still exist.
- **Current state:** each accepted task's current state follows the acknowledgement, so a task that finished before the stream opened is not waited on forever.
- **Changes:** every later change arrives as `notifications/tasks`, carrying the full task, exactly as `tasks/get` would return it.
- **Slow readers:** a stream that falls behind gets each task's latest state rather than every state in between, so its queue cannot grow without bound.

A server without tasks ignores `taskIds`, as it always has.

The Go client polls. Notifications are for clients that keep a stream open anyway.

## Not covered

- The 2025-11-25 core tasks (`tasks/list`, `tasks/result`, task-augmented requests) are not implemented. The extension replaced them.
- Only `tools/call` can be answered with a task, which is what the extension defines today.
