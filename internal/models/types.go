package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Connection represents a WebSocket connection
type Connection struct {
	ID       string    `json:"id"`
	UserID   string    `json:"user_id"`
	RoomID   string    `json:"room_id"`
	DeviceID string    `json:"device_id"`
	Platform string    `json:"platform"` // "firetv", "mobile", "web"
	JoinedAt time.Time `json:"joined_at"`
}

// Room represents a viewing room
type Room struct {
	ID        string                 `json:"id"`
	Code      string                 `json:"code"` // Unique code for the room
	Name      string                 `json:"name"`
	CreatedBy string                 `json:"created_by"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
	MaxUsers  int                    `json:"max_users"`
	IsActive  bool                   `json:"is_active"`
	Metadata  map[string]interface{} `json:"metadata"`
	Users     []User                 `json:"users"`
}

// User represents a user in the system
type User struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Platform string    `json:"platform"`
	JoinedAt time.Time `json:"joined_at"`
	IsHost   bool      `json:"is_host"`
}

// Message types for WebSocket communication
type MessageType string

const (
	// WebRTC Signaling
	MessageTypeOffer  MessageType = "offer"
	MessageTypeAnswer MessageType = "answer"
	MessageTypeICE    MessageType = "ice_candidate"

	// Room Management
	MessageTypeJoinRoom  MessageType = "join_room"
	MessageTypeLeaveRoom MessageType = "leave_room"
	MessageTypeRoomState MessageType = "room_state"

	// User Events
	MessageTypeUserJoined MessageType = "user_joined"
	MessageTypeUserLeft   MessageType = "user_left"

	// System
	MessageTypeError MessageType = "error"
	MessageTypePing  MessageType = "ping"
	MessageTypePong  MessageType = "pong"
)

// WebSocketMessage represents a message sent over WebSocket
type WebSocketMessage struct {
	Type      MessageType            `json:"type"`
	RoomID    string                 `json:"room_id,omitempty"`
	UserID    string                 `json:"user_id,omitempty"`
	TargetID  string                 `json:"target_id,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
	MessageID string                 `json:"message_id"`
}

// WebRTC specific message types
type WebRTCOffer struct {
	SDP    string `json:"sdp"`
	Type   string `json:"type"`
	UserID string `json:"user_id"`
}

type WebRTCAnswer struct {
	SDP    string `json:"sdp"`
	Type   string `json:"type"`
	UserID string `json:"user_id"`
}

type WebRTCICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
	SDPMid        string `json:"sdpMid"`
	UserID        string `json:"user_id"`
}

// Redis message for pub/sub
type RedisMessage struct {
	Channel   string           `json:"channel"`
	Message   WebSocketMessage `json:"message"`
	Timestamp time.Time        `json:"timestamp"`
}

// Helper functions
func NewConnection(userID, roomID, deviceID, platform string) *Connection {
	return &Connection{
		ID:       uuid.New().String(),
		UserID:   userID,
		RoomID:   roomID,
		DeviceID: deviceID,
		Platform: platform,
		JoinedAt: time.Now(),
	}
}

func NewRoom(name, createdBy string, maxUsers int) *Room {
	return &Room{
		ID:        uuid.New().String(),
		Name:      name,
		CreatedBy: createdBy,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		MaxUsers:  maxUsers,
		IsActive:  true,
		Metadata:  make(map[string]interface{}),
		Users:     make([]User, 0),
	}
}

func NewWebSocketMessage(msgType MessageType, roomID, userID string, data map[string]interface{}) *WebSocketMessage {
	return &WebSocketMessage{
		Type:      msgType,
		RoomID:    roomID,
		UserID:    userID,
		Data:      data,
		Timestamp: time.Now(),
		MessageID: uuid.New().String(),
	}
}

func (m *WebSocketMessage) ToJSON() ([]byte, error) {
	return json.Marshal(m)
}

func (m *WebSocketMessage) FromJSON(data []byte) error {
	return json.Unmarshal(data, m)
}
