//go:build tck

// Command a2abridge-tck serves the a2abridge A2A HTTP stack — the same
// handler assembly, task store and push sender the bridge uses — with a
// scenario executor driven by the official A2A TCK message-id prefixes
// (a2a-tck scenarios/core_operations.feature and streaming.feature).
//
//	go build -tags tck ./cmd/a2abridge-tck
//	./a2abridge-tck --addr 127.0.0.1:9999
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"

	"github.com/vbcherepanov/a2abridge/v4/internal/agent"
	"github.com/vbcherepanov/a2abridge/v4/internal/buildinfo"
)

const (
	defaultAddr             = "127.0.0.1:9999"
	defaultStreamingTimeout = 2.0 // seconds, the TCK_STREAMING_TIMEOUT default
	resubscribeHoldFactor   = 2   // test-resubscribe-message-id stays WORKING this many streaming timeouts
	readHeaderTimeout       = 10 * time.Second
	requestReadTimeout      = time.Minute
	shutdownTimeout         = 3 * time.Second
)

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("a2abridge-tck", flag.ContinueOnError)
	addr := fs.String("addr", envOr("SUT_HOST", defaultAddr), "listen address; the Agent Card advertises http://<addr>")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	holdFor, err := resubscribeHold(os.Getenv("TCK_STREAMING_TIMEOUT"))
	if err != nil {
		log.Error("TCK_STREAMING_TIMEOUT", "err", err)
		return 2
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	baseURL := "http://" + ln.Addr().String()

	card := &a2a.AgentCard{
		Name:        "a2abridge TCK System Under Test",
		Description: "a2abridge HTTP stack with the A2A TCK scenario executor",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(baseURL, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(baseURL, a2a.TransportProtocolHTTPJSON),
		},
		Version:            buildinfo.Get().Version,
		Capabilities:       a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:  []string{agent.TextMediaType},
		DefaultOutputModes: []string{agent.TextMediaType},
		Skills: []a2a.AgentSkill{{
			ID:          "tck",
			Name:        "TCK Conformance",
			Description: "Handles TCK conformance test messages",
			Tags:        []string{"tck"},
		}},
		Provider: &a2a.AgentProvider{Org: "a2abridge", URL: "https://github.com/vbcherepanov/a2abridge"},
	}

	pushConfigs := push.NewInMemoryStore()
	tasks := agent.NewTTLStore(pushConfigs, log)
	defer tasks.Close()
	sender := agent.NewPushSender(nil, true, log)
	executor := &scenarioExecutor{holdFor: holdFor}

	handler, err := agent.NewA2AHTTPHandler(card, executor, tasks, pushConfigs, sender, log)
	if err != nil {
		log.Error("a2a handler", "err", err)
		return 1
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       requestReadTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("tck sut listening", "url", baseURL, "resubscribe_hold", holdFor)
		errc <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			return 1
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer drainCancel()
	if err := sender.Close(drainCtx); err != nil {
		log.Warn("push drain", "err", err)
	}
	return 0
}

// resubscribeHold returns how long the resubscribe scenario stays WORKING.
func resubscribeHold(raw string) (time.Duration, error) {
	seconds := defaultStreamingTimeout
	if raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 {
			return 0, fmt.Errorf("want positive seconds, got %q", raw)
		}
		seconds = v
	}
	return time.Duration(seconds * resubscribeHoldFactor * float64(time.Second)), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// step produces the events of one scenario after the task exists.
type step func(ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool)

type scenario struct {
	prefix string
	// messageOnly scenarios answer with a Message and never create a task.
	messageOnly bool
	run         step
}

// scenarioExecutor implements the TCK SUT behaviour keyed by messageId prefix.
type scenarioExecutor struct {
	holdFor time.Duration
}

var _ a2asrv.AgentExecutor = (*scenarioExecutor)(nil)

// scenarios is ordered so that longer prefixes win over their own prefixes
// (tck-artifact-file-url before tck-artifact-file).
func (x *scenarioExecutor) scenarios() []scenario {
	return []scenario{
		{prefix: "tck-complete-task", run: completeWithMessage("Hello from TCK")},
		{prefix: "tck-artifact-text", run: sequence(artifact(a2a.NewTextPart("Generated text content")), status(a2a.TaskStateCompleted))},
		{prefix: "tck-artifact-file-url", run: sequence(artifact(fileURLPart()), status(a2a.TaskStateCompleted))},
		{prefix: "tck-artifact-file", run: sequence(artifact(filePart()), status(a2a.TaskStateCompleted))},
		{prefix: "tck-artifact-data", run: sequence(artifact(dataPart()), status(a2a.TaskStateCompleted))},
		{prefix: "tck-message-response", messageOnly: true, run: messageResponse("Direct message response")},
		{prefix: "tck-input-required", run: status(a2a.TaskStateInputRequired)},
		{prefix: "tck-reject-task", run: fail("rejected")},
		{prefix: "tck-stream-001", run: workingArtifactCompleted("Stream hello from TCK")},
		{prefix: "tck-stream-002", run: status(a2a.TaskStateCompleted)},
		{prefix: "tck-stream-003", run: workingArtifactCompleted("Stream task lifecycle")},
		{prefix: "tck-stream-ordering-001", run: workingArtifactCompleted("Ordered output")},
		{prefix: "tck-stream-artifact-text", run: workingArtifactCompleted("Streamed text content")},
		{prefix: "tck-stream-artifact-file", run: sequence(status(a2a.TaskStateWorking), artifact(filePart()), status(a2a.TaskStateCompleted))},
		{prefix: "tck-stream-artifact-chunked", run: sequence(status(a2a.TaskStateWorking), chunkedArtifact("chunk-1 ", "chunk-2"), status(a2a.TaskStateCompleted))},
		{prefix: "test-resubscribe-message-id", run: sequence(status(a2a.TaskStateWorking), hold(x.holdFor), status(a2a.TaskStateCompleted))},
	}
}

// Execute implements a2asrv.AgentExecutor.
func (x *scenarioExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.Message == nil {
			yield(nil, fmt.Errorf("message is required: %w", a2a.ErrInvalidParams))
			return
		}
		sc := x.match(ec.Message.ID)
		if !sc.messageOnly && ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		sc.run(ctx, ec, yield)
	}
}

// Cancel implements a2asrv.AgentExecutor.
func (*scenarioExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (x *scenarioExecutor) match(messageID string) scenario {
	for _, sc := range x.scenarios() {
		if strings.HasPrefix(messageID, sc.prefix) {
			return sc
		}
	}
	return scenario{run: completeWithMessage("Unhandled messageId prefix: " + messageID)}
}

func sequence(steps ...step) step {
	return func(ctx context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		for _, s := range steps {
			stopped := false
			s(ctx, ec, func(ev a2a.Event, err error) bool {
				if !yield(ev, err) || err != nil {
					stopped = true
					return false
				}
				return true
			})
			if stopped || ctx.Err() != nil {
				return
			}
		}
	}
}

func status(state a2a.TaskState) step {
	return func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, state, nil), nil)
	}
}

func completeWithMessage(text string) step {
	return func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(text))
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, msg), nil)
	}
}

func workingArtifactCompleted(text string) step {
	return sequence(status(a2a.TaskStateWorking), artifact(a2a.NewTextPart(text)), status(a2a.TaskStateCompleted))
}

func artifact(part *a2a.Part) step {
	return func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(a2a.NewArtifactEvent(ec, part), nil)
	}
}

func chunkedArtifact(first, last string) step {
	return func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		head := a2a.NewArtifactEvent(ec, a2a.NewTextPart(first))
		if !yield(head, nil) {
			return
		}
		tail := a2a.NewArtifactUpdateEvent(ec, head.Artifact.ID, a2a.NewTextPart(last))
		tail.LastChunk = true
		yield(tail, nil)
	}
}

func messageResponse(text string) step {
	return func(_ context.Context, ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(text))
		msg.ContextID = ec.ContextID
		yield(msg, nil)
	}
}

// fail ends the execution with an error; a2a-go moves the task to FAILED,
// matching the TCK's generated "reject with error" behaviour.
func fail(reason string) step {
	return func(_ context.Context, _ *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) {
		yield(nil, errors.New(reason))
	}
}

func hold(d time.Duration) step {
	return func(ctx context.Context, _ *a2asrv.ExecutorContext, _ func(a2a.Event, error) bool) {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}

func filePart() *a2a.Part {
	return &a2a.Part{Content: a2a.Raw("tck"), Filename: "output.txt", MediaType: agent.TextMediaType}
}

func fileURLPart() *a2a.Part {
	part := a2a.NewFileURLPart("https://example.com/output.txt", agent.TextMediaType)
	part.Filename = "output.txt"
	return part
}

func dataPart() *a2a.Part {
	return a2a.NewDataPart(map[string]any{"key": "value", "count": 42})
}
