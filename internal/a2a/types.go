// Package a2a contains types per the A2A protocol specification
// (https://a2a-protocol.org/latest/specification/).
package a2a

import (
	"encoding/json"
	"time"
)

// WellKnownPath is the URL where the Agent Card is published.
const WellKnownPath = "/.well-known/a2a"

// TaskState per A2A spec §6.4.
type TaskState string

const (
	TaskStateUnspecified   TaskState = "TASK_STATE_UNSPECIFIED"
	TaskStateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking       TaskState = "TASK_STATE_WORKING"
	TaskStateCompleted     TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed        TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled      TaskState = "TASK_STATE_CANCELED"
	TaskStateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateRejected      TaskState = "TASK_STATE_REJECTED"
	TaskStateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// Role per A2A spec §6.2.
type Role string

const (
	RoleUser  Role = "ROLE_USER"
	RoleAgent Role = "ROLE_AGENT"
)

// AgentCard — the metadata document published at /.well-known/a2a.
type AgentCard struct {
	ProtocolVersion    string                `json:"protocolVersion"`
	Name               string                `json:"name"`
	Description        string                `json:"description,omitempty"`
	URL                string                `json:"url"` // base endpoint for JSON-RPC
	PreferredTransport string                `json:"preferredTransport,omitempty"`
	Provider           *AgentProvider        `json:"provider,omitempty"`
	Version            string                `json:"version"`
	Capabilities       AgentCapabilities     `json:"capabilities"`
	SecuritySchemes    map[string]any        `json:"securitySchemes,omitempty"`
	Security           []map[string][]string `json:"security,omitempty"`
	DefaultInputModes  []string              `json:"defaultInputModes,omitempty"`
	DefaultOutputModes []string              `json:"defaultOutputModes,omitempty"`
	Skills             []AgentSkill          `json:"skills,omitempty"`
}

type AgentProvider struct {
	Organization string `json:"organization"`
	URL          string `json:"url,omitempty"`
}

type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
	ExtendedAgentCard bool `json:"extendedAgentCard,omitempty"`
}

type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// Part — one-of text/raw/url/data. Exactly one field is non-nil.
type Part struct {
	Text      string          `json:"text,omitempty"`
	Raw       []byte          `json:"raw,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// Message per A2A spec §6.2.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             Role           `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// Artifact per A2A spec §6.6.
type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// TaskStatus per A2A spec §6.3.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Task per A2A spec §6.1.
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Kind      string         `json:"kind,omitempty"` // "task"
}

// --- JSON-RPC params & results ---

// MessageSendConfiguration per §7.1.
type MessageSendConfiguration struct {
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	Blocking            bool     `json:"blocking,omitempty"`
	HistoryLength       int      `json:"historyLength,omitempty"`
}

// MessageSendParams — params for a2a.SendMessage / a2a.SendStreamingMessage.
type MessageSendParams struct {
	Message       Message                   `json:"message"`
	Configuration *MessageSendConfiguration `json:"configuration,omitempty"`
	Metadata      map[string]any            `json:"metadata,omitempty"`
}

// TaskIDParams — params for a2a.GetTask / a2a.CancelTask / a2a.SubscribeToTask.
type TaskIDParams struct {
	ID            string `json:"id"`
	HistoryLength int    `json:"historyLength,omitempty"`
}

// StreamResponse — union event emitted on SSE channel.
type StreamResponse struct {
	Task           *Task                    `json:"task,omitempty"`
	Message        *Message                 `json:"message,omitempty"`
	StatusUpdate   *TaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

type TaskStatusUpdateEvent struct {
	TaskID    string     `json:"taskId"`
	ContextID string     `json:"contextId,omitempty"`
	Status    TaskStatus `json:"status"`
	Final     bool       `json:"final"`
}

type TaskArtifactUpdateEvent struct {
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId,omitempty"`
	Artifact  Artifact `json:"artifact"`
	Append    bool     `json:"append,omitempty"`
	LastChunk bool     `json:"lastChunk,omitempty"`
}

// --- JSON-RPC envelope ---

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Method names per A2A spec §7 (JSON-RPC 2.0 binding).
const (
	MethodSendMessage          = "a2a.SendMessage"
	MethodSendStreamingMessage = "a2a.SendStreamingMessage"
	MethodGetTask              = "a2a.GetTask"
	MethodListTasks            = "a2a.ListTasks"
	MethodCancelTask           = "a2a.CancelTask"
	MethodSubscribeToTask      = "a2a.SubscribeToTask"
	MethodGetExtendedCard      = "a2a.GetExtendedAgentCard"

	// Push Notification configuration (§9.5).
	MethodCreatePushConfig = "a2a.CreateTaskPushNotificationConfig"
	MethodGetPushConfig    = "a2a.GetTaskPushNotificationConfig"
	MethodListPushConfig   = "a2a.ListTaskPushNotificationConfig"
	MethodDeletePushConfig = "a2a.DeleteTaskPushNotificationConfig"
)

// PushNotificationConfig — peer-supplied webhook for task state updates (§9.5).
type PushNotificationConfig struct {
	ID             string                       `json:"id,omitempty"` // server-assigned
	URL            string                       `json:"url"`
	Token          string                       `json:"token,omitempty"` // shared secret echoed in X-A2A-Token
	Authentication *PushNotificationAuthDetails `json:"authentication,omitempty"`
}

// PushNotificationAuthDetails — optional authentication mode per A2A 1.0.
// Schemes is a list of supported auth scheme names (e.g. "Bearer", "Basic")
// as defined in the agent card's securitySchemes map.
type PushNotificationAuthDetails struct {
	Schemes     []string `json:"schemes"`
	Credentials string   `json:"credentials,omitempty"` // free-form per scheme
}

// TaskPushNotificationConfig wraps a config with its taskId for the
// Create/Get/List endpoints.
type TaskPushNotificationConfig struct {
	TaskID string                 `json:"taskId"`
	Config PushNotificationConfig `json:"pushNotificationConfig"`
}

// PushNotificationConfigParams for Get/Delete by config id.
type PushNotificationConfigParams struct {
	TaskID         string `json:"taskId"`
	PushConfigID   string `json:"pushNotificationConfigId,omitempty"`
}

// Error codes per A2A spec §8.
const (
	ErrCodeParse          = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603

	ErrCodeTaskNotFound                 = -32001
	ErrCodeTaskNotCancelable            = -32002
	ErrCodePushNotificationNotSupported = -32003
	ErrCodeUnsupportedOperation         = -32004
	ErrCodeContentTypeNotSupported      = -32005
	ErrCodeInvalidAgentResponse         = -32006
	ErrCodeVersionNotSupported          = -32007
)

const ProtocolVersion = "1.0"
