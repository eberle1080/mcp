package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/authorization"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
)

// TaskOption configures the task runtime enabled by WithTasks.
type TaskOption func(*taskManager)

// WithTaskTTL sets how long after creation a task is kept (default one hour).
// After it the task is discarded, and stopped if it is still running.
func WithTaskTTL(ttl time.Duration) TaskOption {
	return func(m *taskManager) {
		if ttl > 0 {
			m.ttl = ttl
		}
	}
}

// WithTaskPollInterval sets the poll interval suggested to clients (default
// one second).
func WithTaskPollInterval(interval time.Duration) TaskOption {
	return func(m *taskManager) {
		if interval > 0 {
			m.poll = interval
		}
	}
}

// WithMaxTasks caps how many tasks the server keeps at once (default 10,000).
// At the cap, tasks.Start returns an error and the tool decides what to do.
func WithMaxTasks(limit int) TaskOption {
	return func(m *taskManager) {
		if limit > 0 {
			m.limit = limit
		}
	}
}

// WithMaxTasksPerCaller caps how many tasks one caller (see WithTaskOwner)
// holds at once (default 1,000), so one caller cannot fill the store for
// everyone. Without authentication every caller is the same one.
func WithMaxTasksPerCaller(limit int) TaskOption {
	return func(m *taskManager) {
		if limit > 0 {
			m.perCaller = limit
		}
	}
}

// WithTaskOwner sets how a request's caller is identified. Every task is bound
// to the caller that created it, and is "not found" for any other. The default
// is a digest of the request's bearer token, which binds a task to the exact
// token; a server whose tokens are refreshed during a task should identify the
// caller by a stable subject instead. Without a token every caller is the same
// one, and a task's unguessable ID is all that protects it.
func WithTaskOwner(owner func(ctx context.Context) string) TaskOption {
	return func(m *taskManager) {
		if owner != nil {
			m.owner = owner
		}
	}
}

// WithTasks enables the Tasks extension (io.modelcontextprotocol/tasks, MCP
// 2026-07-28). The server declares it in server/discover and serves tasks/get,
// tasks/update and tasks/cancel, and a tool may answer a tools/call with a task
// by calling tasks.Start from the mcp-protocol tasks package. Tasks live in
// this process's memory: they do not survive a restart, and a deployment with
// several instances must route requests by their Mcp-Name header (the task ID).
func WithTasks(options ...TaskOption) Option {
	return func(s *Server) error {
		manager := newTaskManager()
		for _, option := range options {
			option(manager)
		}
		s.tasks = manager
		return nil
	}
}

// defaultTaskOwner identifies the caller by its bearer token. The HTTP
// middleware stores the whole Authorization header ("Bearer ...") and the
// JSON-RPC authorizer the bare token, so the scheme is dropped first.
func defaultTaskOwner(ctx context.Context) string {
	token, ok := ctx.Value(authorization.TokenKey).(*authorization.Token)
	if !ok || token == nil {
		return ""
	}
	fields := strings.Fields(token.Token)
	if len(fields) > 0 && strings.EqualFold(fields[0], "bearer") {
		fields = fields[1:]
	}
	value := strings.Join(fields, " ")
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

type taskManager struct {
	mu        sync.Mutex
	tasks     map[string]*taskState
	perOwner  map[string]int
	listeners map[*taskListener]struct{}
	ttl       time.Duration
	poll      time.Duration
	limit     int
	perCaller int
	owner     func(context.Context) string
	now       func() time.Time
}

func newTaskManager() *taskManager {
	return &taskManager{
		tasks:     map[string]*taskState{},
		perOwner:  map[string]int{},
		listeners: map[*taskListener]struct{}{},
		ttl:       time.Hour,
		poll:      time.Second,
		limit:     10000,
		perCaller: 1000,
		owner:     defaultTaskOwner,
		now:       time.Now,
	}
}

type taskState struct {
	task    tasks.Task
	owner   string
	expires time.Time
	// ctx is the work's context: done when the task is cancelled or expires.
	ctx         context.Context
	cancel      context.CancelFunc
	expiry      *time.Timer
	cancelled   bool
	nextKey     int
	outstanding map[string]tasks.InputRequest
	waiters     map[string]chan json.RawMessage
}

// taskListener is one subscriptions/listen stream following some tasks.
type taskListener struct {
	owner  string
	ids    map[string]bool
	notify func(tasks.Task)
}

func (m *taskManager) timestamp() string {
	return m.now().UTC().Format(time.RFC3339Nano)
}

// removeLocked discards a task, stopping its work if it is still running.
func (m *taskManager) removeLocked(id string, state *taskState) {
	state.cancel()
	if state.expiry != nil {
		state.expiry.Stop()
	}
	delete(m.tasks, id)
	if m.perOwner[state.owner]--; m.perOwner[state.owner] <= 0 {
		delete(m.perOwner, state.owner)
	}
}

// expire runs when a task's TTL elapses.
func (m *taskManager) expire(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.tasks[id]; ok {
		m.removeLocked(id, state)
	}
}

func newTaskID() (string, error) {
	var raw [16]byte // 128 bits: unguessable
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// start creates a task and runs work. The task is in the store before start
// returns, so a tasks/get for it resolves as soon as the client has its ID.
func (m *taskManager) start(ctx context.Context, work tasks.Work, options tasks.StartOptions) (tasks.Task, error) {
	id, err := newTaskID()
	if err != nil {
		return tasks.Task{}, err
	}
	ttl, poll := m.ttl, m.poll
	if options.TTL > 0 {
		ttl = options.TTL
	}
	if options.PollInterval > 0 {
		poll = options.PollInterval
	}
	ttlMs, pollMs := ttl.Milliseconds(), poll.Milliseconds()
	// The work outlives the request: keep its values, drop its cancellation.
	workCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	owner := m.owner(ctx)
	m.mu.Lock()
	// At a cap the request cannot take a task, so the tool can fall back.
	if len(m.tasks) >= m.limit {
		m.mu.Unlock()
		cancel()
		return tasks.Task{}, fmt.Errorf("%w: the server already holds %d tasks", tasks.ErrUnavailable, m.limit)
	}
	if m.perOwner[owner] >= m.perCaller {
		m.mu.Unlock()
		cancel()
		return tasks.Task{}, fmt.Errorf("%w: this caller already holds %d tasks", tasks.ErrUnavailable, m.perCaller)
	}
	now := m.timestamp()
	state := &taskState{
		task: tasks.Task{
			TaskID: id, Status: tasks.Working, StatusMessage: options.StatusMessage,
			CreatedAt: now, LastUpdatedAt: now, TTLMs: &ttlMs, PollIntervalMs: &pollMs,
		},
		owner:       owner,
		expires:     m.now().Add(ttl),
		ctx:         workCtx,
		cancel:      cancel,
		outstanding: map[string]tasks.InputRequest{},
		waiters:     map[string]chan json.RawMessage{},
	}
	m.tasks[id] = state
	m.perOwner[owner]++
	state.expiry = time.AfterFunc(ttl, func() { m.expire(id) })
	snapshot := copyTask(state.task)
	m.mu.Unlock()

	go m.run(workCtx, id, work)
	return snapshot, nil
}

func (m *taskManager) run(ctx context.Context, id string, work tasks.Work) {
	var (
		result *schema.CallToolResult
		rpcErr *jsonrpc.Error
	)
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				rpcErr = jsonrpc.NewInternalError(fmt.Sprintf("task failed: %v", recovered), nil)
			}
		}()
		result, rpcErr = work(ctx, &taskControl{manager: m, id: id})
	}()
	m.finish(id, result, rpcErr)
}

func (m *taskManager) finish(id string, result *schema.CallToolResult, rpcErr *jsonrpc.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.tasks[id]
	if !ok {
		return
	}
	state.cancel()
	state.task.InputRequests = nil
	state.outstanding = map[string]tasks.InputRequest{}
	switch {
	case state.cancelled:
		state.task.Status = tasks.Cancelled
	case rpcErr != nil:
		state.task.Status = tasks.Failed
		state.task.Error = rpcErr
		if state.task.StatusMessage == "" {
			state.task.StatusMessage = rpcErr.Message
		}
	default:
		if result == nil {
			result = &schema.CallToolResult{}
		}
		if result.Content == nil {
			result.Content = []schema.CallToolResultContentElem{}
		}
		if result.ResultType == "" {
			result.ResultType = schema.ResultTypeComplete
		}
		raw, err := json.Marshal(result)
		if err != nil {
			state.task.Status = tasks.Failed
			state.task.Error = jsonrpc.NewInternalError(fmt.Sprintf("task result: %v", err), nil)
		} else {
			state.task.Status = tasks.Completed
			state.task.Result = raw
		}
	}
	state.task.LastUpdatedAt = m.timestamp()
	m.publishLocked(state)
}

// lookupLocked finds a task the caller may see. Another caller's task, an
// expired one and an unknown ID are all "not found".
func (m *taskManager) lookupLocked(ctx context.Context, id string) (*taskState, *jsonrpc.Error) {
	state, ok := m.tasks[id]
	if ok && m.now().After(state.expires) {
		// The timer has not run yet; the task is gone all the same.
		m.removeLocked(id, state)
		ok = false
	}
	if !ok || state.owner != m.owner(ctx) {
		return nil, jsonrpc.NewInvalidParamsError("Failed to retrieve task: Task not found", nil)
	}
	return state, nil
}

func (m *taskManager) get(ctx context.Context, id string) (tasks.Task, *jsonrpc.Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, rpcErr := m.lookupLocked(ctx, id)
	if rpcErr != nil {
		return tasks.Task{}, rpcErr
	}
	return copyTask(state.task), nil
}

// update delivers answers to outstanding input requests. Keys that are not
// outstanding (never issued, already answered, superseded) are ignored.
func (m *taskManager) update(ctx context.Context, id string, responses map[string]json.RawMessage) *jsonrpc.Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, rpcErr := m.lookupLocked(ctx, id)
	if rpcErr != nil {
		return rpcErr
	}
	answered := false
	for key, response := range responses {
		waiter, ok := state.waiters[key]
		if !ok {
			continue
		}
		delete(state.outstanding, key)
		delete(state.waiters, key)
		waiter <- append(json.RawMessage(nil), response...)
		answered = true
	}
	if answered {
		m.inputChangedLocked(state)
	}
	return nil
}

// cancel asks the task to stop. It is cooperative: the work's context is
// cancelled, and the task ends cancelled when the work returns.
func (m *taskManager) cancelTask(ctx context.Context, id string) *jsonrpc.Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, rpcErr := m.lookupLocked(ctx, id)
	if rpcErr != nil {
		return rpcErr
	}
	if !state.task.Status.Terminal() {
		state.cancelled = true
		state.cancel()
	}
	return nil
}

// inputChangedLocked recomputes a running task's status from its outstanding
// input requests.
func (m *taskManager) inputChangedLocked(state *taskState) {
	if state.task.Status.Terminal() {
		return
	}
	if len(state.outstanding) == 0 {
		state.task.Status = tasks.Working
		state.task.InputRequests = nil
	} else {
		state.task.Status = tasks.InputRequired
		state.task.InputRequests = make(map[string]tasks.InputRequest, len(state.outstanding))
		for key, request := range state.outstanding {
			state.task.InputRequests[key] = request
		}
	}
	state.task.LastUpdatedAt = m.timestamp()
	m.publishLocked(state)
}

// publishLocked hands the task's state to the streams following it. A
// listener's notify only queues the state, so a slow stream never holds the
// store.
func (m *taskManager) publishLocked(state *taskState) {
	snapshot := copyTask(state.task)
	var targets []*taskListener
	for listener := range m.listeners {
		if listener.owner == state.owner && listener.ids[state.task.TaskID] {
			targets = append(targets, listener)
		}
	}
	for _, listener := range targets {
		listener.notify(snapshot)
	}
}

// listen follows the given tasks for one stream: the caller's own tasks that
// still exist. It hands each one's current state to notify before any later
// change, under the store's lock, so a stream never ends on a stale state.
// It returns the accepted IDs and a function that stops following them.
func (m *taskManager) listen(ctx context.Context, ids []string, notify func(tasks.Task)) ([]string, func()) {
	owner := m.owner(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	listener := &taskListener{owner: owner, ids: map[string]bool{}, notify: notify}
	accepted := []string{}
	for _, id := range ids {
		state, ok := m.tasks[id]
		if !ok || state.owner != owner || listener.ids[id] || m.now().After(state.expires) {
			continue
		}
		listener.ids[id] = true
		accepted = append(accepted, id)
		notify(copyTask(state.task))
	}
	if len(accepted) == 0 {
		return accepted, func() {}
	}
	m.listeners[listener] = struct{}{}
	var once sync.Once
	return accepted, func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.listeners, listener)
			m.mu.Unlock()
		})
	}
}

func copyTask(task tasks.Task) tasks.Task {
	if task.TTLMs != nil {
		ttl := *task.TTLMs
		task.TTLMs = &ttl
	}
	if task.PollIntervalMs != nil {
		poll := *task.PollIntervalMs
		task.PollIntervalMs = &poll
	}
	if task.InputRequests != nil {
		requests := make(map[string]tasks.InputRequest, len(task.InputRequests))
		for key, request := range task.InputRequests {
			requests[key] = request
		}
		task.InputRequests = requests
	}
	task.Result = append(json.RawMessage(nil), task.Result...)
	if len(task.Result) == 0 {
		task.Result = nil
	}
	if task.Error != nil {
		copy := *task.Error
		task.Error = &copy
	}
	return task
}

// taskControl is the tasks.Control a task's work receives.
type taskControl struct {
	manager *taskManager
	id      string
}

func (c *taskControl) ID() string { return c.id }

func (c *taskControl) Progress(message string) {
	m := c.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.tasks[c.id]
	if !ok || state.task.Status.Terminal() {
		return
	}
	state.task.StatusMessage = message
	state.task.LastUpdatedAt = m.timestamp()
	m.publishLocked(state)
}

// errTaskGone means the task was discarded while its work waited for input.
var errTaskGone = errors.New("tasks: the task no longer exists")

func (c *taskControl) Input(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	m := c.manager
	m.mu.Lock()
	state, ok := m.tasks[c.id]
	if !ok || state.task.Status.Terminal() {
		m.mu.Unlock()
		return nil, errTaskGone
	}
	// Keys are never reused over the task's life.
	state.nextKey++
	key := "input-" + strconv.Itoa(state.nextKey)
	waiter := make(chan json.RawMessage, 1)
	state.outstanding[key] = tasks.InputRequest{Method: method, Params: raw}
	state.waiters[key] = waiter
	m.inputChangedLocked(state)
	taskCtx := state.ctx
	m.mu.Unlock()

	// The work may wait on a context of its own; the task's ends the wait too,
	// so a cancelled or expired task never waits for input forever.
	done := ctx.Done()
	stopped := ctx.Err
	if taskCtx != nil && taskCtx != ctx {
		merged, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(taskCtx, cancel)
		defer stop()
		done, stopped = merged.Done(), func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return taskCtx.Err()
		}
	}
	select {
	case response := <-waiter:
		return response, nil
	case <-done:
		m.mu.Lock()
		if state, ok := m.tasks[c.id]; ok {
			if _, pending := state.waiters[key]; pending {
				delete(state.outstanding, key)
				delete(state.waiters, key)
				m.inputChangedLocked(state)
			}
		}
		m.mu.Unlock()
		// An answer may have arrived just as the context ended.
		select {
		case response := <-waiter:
			return response, nil
		default:
			return nil, stopped()
		}
	}
}

func (c *taskControl) Elicit(ctx context.Context, params *schema.ElicitRequestParams) (*schema.ElicitResult, error) {
	raw, err := c.Input(ctx, schema.MethodElicitationCreate, params)
	if err != nil {
		return nil, err
	}
	var result schema.ElicitResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("tasks: elicitation answer: %w", err)
	}
	return &result, nil
}

func (c *taskControl) CreateMessage(ctx context.Context, params *schema.CreateMessageRequestParams) (*schema.CreateMessageResult, error) {
	raw, err := c.Input(ctx, schema.MethodSamplingCreateMessage, params)
	if err != nil {
		return nil, err
	}
	var result schema.CreateMessageResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("tasks: sampling answer: %w", err)
	}
	return &result, nil
}

// requestTaskRunner is the tasks.Runner for one tools/call. It records the
// task it started so the call can be answered with it.
type requestTaskRunner struct {
	manager *taskManager
	mu      sync.Mutex
	started *tasks.Task
}

func (r *requestTaskRunner) Start(ctx context.Context, work tasks.Work, options tasks.StartOptions) (tasks.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started != nil {
		return tasks.Task{}, errors.New("tasks: this request already started a task")
	}
	task, err := r.manager.start(ctx, work, options)
	if err != nil {
		return tasks.Task{}, err
	}
	r.started = &task
	return task, nil
}

func (r *requestTaskRunner) startedTask() *tasks.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

// requestDeclaresTasks reports whether a 2026-07-28 request declared the
// extension. Older protocol versions have no tasks.
func requestDeclaresTasks(ctx context.Context) bool {
	request, ok := ProtocolRequestFromContext(ctx)
	return ok && request.Version == schema.LatestProtocolVersion && tasks.ClientSupports(&request.Capabilities)
}
