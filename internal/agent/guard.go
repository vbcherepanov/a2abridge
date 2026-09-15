package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

const (
	jsonrpcVersion        = "2.0"
	methodSubscribeToTask = "SubscribeToTask"
	restTasksPrefix       = "/tasks/"
	subscribeActionSuffix = ":subscribe"

	internalRPCCode    = -32603
	internalGRPCStatus = "INTERNAL"

	// impliedProtocolVersion is the version the spec assumes when a request
	// carries no A2A-Version.
	impliedProtocolVersion = "0.3"
)

// protocolErrors maps the errors the guard emits onto both bindings, with the
// same codes and statuses as a2a-go's own JSON-RPC and REST error tables
// (spec §5.4), so clients cannot tell the guard from the SDK.
var protocolErrors = []struct {
	err        error
	rpcCode    int
	httpStatus int
	grpcStatus string
}{
	{a2a.ErrInvalidRequest, -32600, http.StatusBadRequest, "INVALID_ARGUMENT"},
	{a2a.ErrTaskNotFound, -32001, http.StatusNotFound, "NOT_FOUND"},
	{a2a.ErrUnsupportedOperation, -32004, http.StatusBadRequest, "FAILED_PRECONDITION"},
	{a2a.ErrVersionNotSupported, -32009, http.StatusBadRequest, "FAILED_PRECONDITION"},
}

type protocolErrorCodes struct {
	rpcCode    int
	httpStatus int
	grpcStatus string
	reason     string
}

func codesFor(err error) protocolErrorCodes {
	for _, m := range protocolErrors {
		if errors.Is(err, m.err) {
			return protocolErrorCodes{rpcCode: m.rpcCode, httpStatus: m.httpStatus, grpcStatus: m.grpcStatus, reason: a2a.ErrorReason(m.err)}
		}
	}
	return protocolErrorCodes{
		rpcCode:    internalRPCCode,
		httpStatus: http.StatusInternalServerError,
		grpcStatus: internalGRPCStatus,
		reason:     a2a.ErrorReason(a2a.ErrInternalError),
	}
}

// errorDetails builds the google.rpc.ErrorInfo detail a2a-go attaches to
// every error response.
func errorDetails(reason, taskID string) []*errordetails.Typed {
	meta := map[string]string{"timestamp": time.Now().UTC().Format(time.RFC3339)}
	if taskID != "" {
		meta["taskId"] = taskID
	}
	return []*errordetails.Typed{errordetails.NewErrorInfo(reason, a2a.ProtocolDomain, meta)}
}

type rpcErrorBody struct {
	Code    int                   `json:"code"`
	Message string                `json:"message"`
	Data    []*errordetails.Typed `json:"data,omitempty"`
}

type rpcErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcErrorBody    `json:"error"`
}

type restErrorStatus struct {
	Code    int                   `json:"code"`
	Status  string                `json:"status"`
	Message string                `json:"message"`
	Details []*errordetails.Typed `json:"details,omitempty"`
}

type restErrorResponse struct {
	Error restErrorStatus `json:"error"`
}

// rpcEnvelope is the part of a JSON-RPC request the guard needs. The id stays
// raw so it is echoed byte for byte, including large integers.
type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// protocolGuard fills gaps in a2a-go's request handling before a request
// reaches the SDK: bounded request bodies, A2A-Version validation, and
// SubscribeToTask answering a missing or finished task with a plain error
// instead of an SSE stream.
type protocolGuard struct {
	tasks     taskstore.Store
	streaming bool
	log       *slog.Logger
}

// jsonrpc wraps the JSON-RPC binding. The body is buffered here, under the
// http.MaxBytesReader limit set around the mux, so an oversized request gets
// 413 before a2a-go decodes anything. Requests the guard does not understand
// are passed through so a2a-go reports them with its own errors.
func (g *protocolGuard) jsonrpc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			status, readErr := bodyReadError(err)
			g.writeJSONRPCError(w, status, nil, readErr)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var env rpcEnvelope
		parsed := json.Unmarshal(body, &env) == nil && env.JSONRPC == jsonrpcVersion
		var id json.RawMessage
		if parsed && isRPCID(env.ID) {
			id = env.ID
		}
		// An unsupported version is refused whatever the body looks like,
		// batch arrays and malformed requests included.
		if versionErr := checkProtocolVersion(r); versionErr != nil {
			g.writeJSONRPCError(w, http.StatusOK, id, versionErr)
			return
		}
		if id == nil || !g.streaming || env.Method != methodSubscribeToTask {
			next.ServeHTTP(w, r)
			return
		}
		var params a2a.SubscribeToTaskRequest
		if json.Unmarshal(env.Params, &params) == nil && params.ID != "" {
			if err := g.checkSubscribable(r.Context(), params.ID); err != nil {
				g.writeJSONRPCError(w, http.StatusOK, id, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rest wraps the HTTP+JSON binding.
func (g *protocolGuard) rest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				status, readErr := bodyReadError(err)
				g.writeRESTError(w, status, readErr, "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		if err := checkProtocolVersion(r); err != nil {
			g.writeRESTError(w, 0, err, "")
			return
		}
		if g.streaming && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
			if id, ok := subscribeTaskID(r.URL.Path); ok {
				if err := g.checkSubscribable(r.Context(), id); err != nil {
					g.writeRESTError(w, 0, err, id)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bodyReadError classifies a failed body read: over the size limit is 413,
// anything else (client gone, broken encoding) is a plain invalid request.
func bodyReadError(err error) (int, error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes: %w", tooLarge.Limit, a2a.ErrInvalidRequest)
	}
	return http.StatusBadRequest, fmt.Errorf("read request body: %w", a2a.ErrInvalidRequest)
}

// checkSubscribable returns TaskNotFound for an unknown task and
// UnsupportedOperation for a terminal one (spec §3.1.6). Store failures are
// left to a2a-go, which reports them itself.
func (g *protocolGuard) checkSubscribable(ctx context.Context, id a2a.TaskID) error {
	stored, err := g.tasks.Get(ctx, id)
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound):
		return fmt.Errorf("subscribe to task %s: %w", id, a2a.ErrTaskNotFound)
	case err != nil:
		g.log.Warn("subscribe pre-check: task lookup failed", "task", id, "err", err)
		return nil
	case stored.Task.Status.State.Terminal():
		return fmt.Errorf("task %s is in terminal state %s: %w", id, stored.Task.Status.State, a2a.ErrUnsupportedOperation)
	default:
		return nil
	}
}

// writeJSONRPCError writes a JSON-RPC error with the given HTTP status
// (JSON-RPC errors normally travel with 200). A nil id is sent as null.
func (g *protocolGuard) writeJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, err error) {
	codes := codesFor(err)
	resp := rpcErrorResponse{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   rpcErrorBody{Code: codes.rpcCode, Message: err.Error(), Data: errorDetails(codes.reason, "")},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
		g.log.Warn("jsonrpc error response write failed", "err", encErr)
	}
}

// writeRESTError writes a google.rpc.Status error. status 0 uses the
// spec §5.4 mapping of err.
func (g *protocolGuard) writeRESTError(w http.ResponseWriter, status int, err error, taskID a2a.TaskID) {
	codes := codesFor(err)
	if status == 0 {
		status = codes.httpStatus
	}
	resp := restErrorResponse{Error: restErrorStatus{
		Code:    status,
		Status:  codes.grpcStatus,
		Message: err.Error(),
		Details: errorDetails(codes.reason, string(taskID)),
	}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(resp); encErr != nil {
		g.log.Warn("rest error response write failed", "err", encErr)
	}
}

// checkProtocolVersion rejects a request whose A2A-Version (header, or the
// request parameter of the same name) is not 1.0 in Major.Minor. The spec
// reads a missing or empty version as 0.3, which this agent does not speak.
// The a2a-go and a2a-sdk clients send the header on every request.
func checkProtocolVersion(r *http.Request) error {
	v := strings.TrimSpace(r.Header.Get(a2a.SvcParamVersion))
	if v == "" {
		v = strings.TrimSpace(r.URL.Query().Get(a2a.SvcParamVersion))
	}
	if v == "" {
		return fmt.Errorf("A2A-Version not sent, so %s is assumed; this agent speaks %s: %w", impliedProtocolVersion, a2a.Version, a2a.ErrVersionNotSupported)
	}
	got, ok := majorMinor(v)
	want, _ := majorMinor(string(a2a.Version))
	if !ok || got != want {
		return fmt.Errorf("A2A-Version %q is not supported, this agent speaks %s: %w", v, a2a.Version, a2a.ErrVersionNotSupported)
	}
	return nil
}

// majorMinor normalizes "1.0" or "1.0.3" to "1.0"; anything else is invalid.
func majorMinor(v string) (string, bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "", false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return "", false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return "", false
	}
	return strconv.Itoa(major) + "." + strconv.Itoa(minor), true
}

// subscribeTaskID extracts the task id from /tasks/{id}:subscribe.
func subscribeTaskID(path string) (a2a.TaskID, bool) {
	rest, ok := strings.CutPrefix(path, restTasksPrefix)
	if !ok || strings.Contains(rest, "/") {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, subscribeActionSuffix)
	if !ok || id == "" {
		return "", false
	}
	return a2a.TaskID(id), true
}

// isRPCID reports whether raw is a JSON-RPC string or number id; anything
// else is left for a2a-go to reject.
func isRPCID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	c := trimmed[0]
	return c == '"' || c == '-' || (c >= '0' && c <= '9')
}
