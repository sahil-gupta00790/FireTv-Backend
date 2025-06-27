package services

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"fire-tv-rooms-ecs/internal/config"
	"fire-tv-rooms-ecs/internal/models"
	"fire-tv-rooms-ecs/internal/utils"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ConnectionManager manages WebSocket connections efficiently
type ConnectionManager struct {
	// Connection storage
	connections     sync.Map // map[string]*WebSocketConnection
	roomConnections sync.Map // map[string]map[string]*WebSocketConnection

	// Services
	redisService *RedisService

	// Configuration
	config *config.Config

	// Metrics
	connectionCount int64
	maxConnections  int64

	// Lifecycle
	ctx        context.Context
	cancel     context.CancelFunc
	shutdownWG sync.WaitGroup

	// Connection pools
	readBufferPool  sync.Pool
	writeBufferPool sync.Pool

	// Rate limiting
	rateLimiter *RateLimiter
}

// WebSocketConnection represents a single WebSocket connection
type WebSocketConnection struct {
	// Connection details
	ID       string
	Conn     net.Conn
	UserID   string
	RoomID   string
	DeviceID string
	Platform string

	// Connection state
	IsActive int32 // atomic
	JoinedAt time.Time
	LastPing time.Time

	// Communication channels
	SendChan  chan []byte
	CloseChan chan struct{}

	// Synchronization
	writeMutex sync.Mutex

	// Context for this connection
	ctx    context.Context
	cancel context.CancelFunc
}

// RateLimiter implements token bucket rate limiting
type RateLimiter struct {
	tokens     int64
	maxTokens  int64
	refillRate int64
	lastRefill int64
	mutex      sync.Mutex
}

// NewConnectionManager creates a new connection manager
func NewConnectionManager(cfg *config.Config, redisService *RedisService) (*ConnectionManager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if redisService == nil {
		return nil, fmt.Errorf("redis service cannot be nil")
	}

	ctx, cancel := context.WithCancel(context.Background())

	cm := &ConnectionManager{
		redisService:   redisService,
		config:         cfg,
		maxConnections: int64(cfg.Server.MaxConnections),
		ctx:            ctx,
		cancel:         cancel,
		rateLimiter:    NewRateLimiter(1000, 100), // 1000 max tokens, 100 per second
	}

	// Initialize buffer pools for memory efficiency
	cm.readBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 4096) // 4KB read buffer
		},
	}

	cm.writeBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 4096) // 4KB write buffer
		},
	}

	// Subscribe to Redis channels for message broadcasting
	if err := cm.setupRedisSubscriptions(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to setup redis subscriptions: %w", err)
	}

	// Start background tasks
	cm.startBackgroundTasks()

	utils.Info("Connection manager initialized",
		zap.Int64("max_connections", cm.maxConnections))

	return cm, nil
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(maxTokens, refillRate int64) *RateLimiter {
	return &RateLimiter{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now().Unix(),
	}
}

// Allow checks if an action is allowed by the rate limiter
func (rl *RateLimiter) Allow() bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	now := time.Now().Unix()
	elapsed := now - rl.lastRefill

	// Refill tokens
	tokensToAdd := elapsed * rl.refillRate
	rl.tokens = min(rl.maxTokens, rl.tokens+tokensToAdd)
	rl.lastRefill = now

	if rl.tokens > 0 {
		rl.tokens--
		return true
	}
	return false
}

// AddConnection adds a new WebSocket connection
func (cm *ConnectionManager) AddConnection(conn net.Conn, userID, roomID, deviceID, platform string) (*WebSocketConnection, error) {
	// Input validation
	if conn == nil {
		return nil, fmt.Errorf("connection cannot be nil")
	}
	if userID == "" {
		return nil, fmt.Errorf("user ID cannot be empty")
	}
	if roomID == "" {
		return nil, fmt.Errorf("room ID cannot be empty")
	}
	if deviceID == "" {
		return nil, fmt.Errorf("device ID cannot be empty")
	}
	if platform == "" {
		return nil, fmt.Errorf("platform cannot be empty")
	}

	// Rate limiting check
	if !cm.rateLimiter.Allow() {
		return nil, fmt.Errorf("rate limit exceeded")
	}

	// Check connection limits
	currentCount := atomic.LoadInt64(&cm.connectionCount)
	if currentCount >= cm.maxConnections {
		return nil, fmt.Errorf("maximum connections reached: %d", cm.maxConnections)
	}

	// Create connection model
	connModel := models.NewConnection(userID, roomID, deviceID, platform)

	// Create WebSocket connection
	ctx, cancel := context.WithCancel(cm.ctx)
	wsConn := &WebSocketConnection{
		ID:        connModel.ID,
		Conn:      conn,
		UserID:    userID,
		RoomID:    roomID,
		DeviceID:  deviceID,
		Platform:  platform,
		IsActive:  1,
		JoinedAt:  time.Now(),
		LastPing:  time.Now(),
		SendChan:  make(chan []byte, 256), // Buffered channel for async sends
		CloseChan: make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}

	// Store connection
	cm.connections.Store(wsConn.ID, wsConn)

	// Add to room connections
	cm.addToRoom(roomID, wsConn)

	// Store in Redis for cross-server communication
	if err := cm.redisService.SetConnectionState(wsConn.ID, connModel); err != nil {
		utils.Error("Failed to store connection state in Redis",
			zap.Error(err),
			zap.String("connection_id", wsConn.ID))
		// Don't fail the connection for Redis errors
	}

	// Increment connection count
	atomic.AddInt64(&cm.connectionCount, 1)

	// Start connection handlers
	cm.shutdownWG.Add(2)
	go cm.handleConnectionRead(wsConn)
	go cm.handleConnectionWrite(wsConn)

	joinMsg := models.NewWebSocketMessage(
		models.MessageTypeUserJoined,
		roomID,
		userID,
		map[string]interface{}{
			"user_id":   userID,
			"platform":  platform,
			"device_id": deviceID,
		},
	)

	go func() {
		time.Sleep(time.Millisecond * 100) // Small delay to ensure connection is ready
		cm.broadcastToRoom(roomID, joinMsg, wsConn.ID)
	}()

	utils.Info("Connection added successfully",
		zap.String("connection_id", wsConn.ID),
		zap.String("user_id", userID),
		zap.String("room_id", roomID),
		zap.String("platform", platform),
		zap.Int64("total_connections", atomic.LoadInt64(&cm.connectionCount)))

	return wsConn, nil
}

// addToRoom adds a connection to a room's connection map
func (cm *ConnectionManager) addToRoom(roomID string, conn *WebSocketConnection) {
	roomConns, _ := cm.roomConnections.LoadOrStore(roomID, &sync.Map{})
	roomMap := roomConns.(*sync.Map)
	roomMap.Store(conn.ID, conn)
}

// removeFromRoom removes a connection from a room's connection map
func (cm *ConnectionManager) removeFromRoom(roomID, connID string) {
	if roomConns, ok := cm.roomConnections.Load(roomID); ok {
		roomMap := roomConns.(*sync.Map)
		roomMap.Delete(connID)

		// Check if room is empty
		isEmpty := true
		roomMap.Range(func(key, value interface{}) bool {
			isEmpty = false
			return false // Stop iteration
		})

		if isEmpty {
			cm.roomConnections.Delete(roomID)
			utils.Debug("Empty room removed", zap.String("room_id", roomID))
		}
	}
}

// RemoveConnection removes a WebSocket connection
func (cm *ConnectionManager) RemoveConnection(connID string) error {
	if connID == "" {
		return fmt.Errorf("connection ID cannot be empty")
	}

	connInterface, ok := cm.connections.Load(connID)
	if !ok {
		return fmt.Errorf("connection not found: %s", connID)
	}

	conn := connInterface.(*WebSocketConnection)

	// Mark as inactive
	atomic.StoreInt32(&conn.IsActive, 0)

	// Remove from storage
	cm.connections.Delete(connID)
	cm.removeFromRoom(conn.RoomID, connID)

	// Close connection
	conn.cancel()
	close(conn.CloseChan)

	if err := conn.Conn.Close(); err != nil {
		utils.Error("Failed to close connection",
			zap.Error(err),
			zap.String("connection_id", connID))
	}

	// Remove from Redis
	if err := cm.redisService.DeleteConnectionState(connID); err != nil {
		utils.Error("Failed to remove connection state from Redis",
			zap.Error(err),
			zap.String("connection_id", connID))
	}

	// Decrement connection count
	atomic.AddInt64(&cm.connectionCount, -1)

	// Notify room about user leaving
	cm.broadcastToRoom(conn.RoomID, models.NewWebSocketMessage(
		models.MessageTypeUserLeft,
		conn.RoomID,
		conn.UserID,
		map[string]interface{}{
			"user_id":  conn.UserID,
			"platform": conn.Platform,
		},
	), connID)

	utils.Info("Connection removed successfully",
		zap.String("connection_id", connID),
		zap.String("user_id", conn.UserID),
		zap.String("room_id", conn.RoomID),
		zap.Int64("total_connections", atomic.LoadInt64(&cm.connectionCount)))

	return nil
}

// handleConnectionRead handles incoming messages from WebSocket connection
func (cm *ConnectionManager) handleConnectionRead(conn *WebSocketConnection) {
	defer cm.shutdownWG.Done()
	defer func() {
		if err := recover(); err != nil {
			utils.Error("Panic in connection read handler",
				zap.String("connection_id", conn.ID),
				zap.Any("error", err))
		}
		cm.RemoveConnection(conn.ID)
	}()

	for {
		select {
		case <-conn.ctx.Done():
			return
		case <-conn.CloseChan:
			return
		default:
			// Set read deadline
			if err := conn.Conn.SetReadDeadline(time.Now().Add(time.Minute * 10)); err != nil {
				utils.Error("Failed to set read deadline",
					zap.Error(err),
					zap.String("connection_id", conn.ID))
				return
			}

			// Read message
			msg, op, err := wsutil.ReadClientData(conn.Conn)
			if err != nil {
				if !isConnectionClosed(err) {
					utils.Error("Failed to read WebSocket message",
						zap.Error(err),
						zap.String("connection_id", conn.ID))
				}
				return
			}

			// Handle different message types
			switch op {
			case ws.OpText:
				if err := cm.handleTextMessage(conn, msg); err != nil {
					utils.Error("Failed to handle text message",
						zap.Error(err),
						zap.String("connection_id", conn.ID))
				}
			case ws.OpPing:
				if err := cm.handlePing(conn, msg); err != nil {
					utils.Error("Failed to handle ping",
						zap.Error(err),
						zap.String("connection_id", conn.ID))
				}
			case ws.OpPong:
				conn.LastPing = time.Now()
			case ws.OpClose:
				return
			}
		}
	}
}

// handleConnectionWrite handles outgoing messages to WebSocket connection
func (cm *ConnectionManager) handleConnectionWrite(conn *WebSocketConnection) {
	defer cm.shutdownWG.Done()
	defer func() {
		if err := recover(); err != nil {
			utils.Error("Panic in connection write handler",
				zap.String("connection_id", conn.ID),
				zap.Any("error", err))
		}
	}()

	ticker := time.NewTicker(time.Second * 30) // Ping every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-conn.ctx.Done():
			return
		case <-conn.CloseChan:
			return
		case <-ticker.C:
			if err := cm.sendPing(conn); err != nil {
				utils.Error("Failed to send ping",
					zap.Error(err),
					zap.String("connection_id", conn.ID))
				return
			}
		case data := <-conn.SendChan:
			if err := cm.writeMessage(conn, data); err != nil {
				utils.Error("Failed to write message",
					zap.Error(err),
					zap.String("connection_id", conn.ID))
				return
			}
		}
	}
}

// handleTextMessage processes incoming text messages
func (cm *ConnectionManager) handleTextMessage(conn *WebSocketConnection, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty message")
	}

	// Parse WebSocket message
	var msg models.WebSocketMessage
	if err := msg.FromJSON(data); err != nil {
		return fmt.Errorf("failed to parse message: %w", err)
	}

	// Validate message
	if err := cm.validateMessage(&msg); err != nil {
		return fmt.Errorf("invalid message: %w", err)
	}

	// Update connection info if needed
	if msg.UserID != "" && msg.UserID != conn.UserID {
		conn.UserID = msg.UserID
	}
	if msg.RoomID != "" && msg.RoomID != conn.RoomID {
		// Handle room change
		cm.removeFromRoom(conn.RoomID, conn.ID)
		conn.RoomID = msg.RoomID
		cm.addToRoom(conn.RoomID, conn)
	}

	// Handle different message types
	switch msg.Type {
	case models.MessageTypeJoinRoom:
		return cm.handleJoinRoom(conn, &msg)
	case models.MessageTypeLeaveRoom:
		return cm.handleLeaveRoom(conn, &msg)
	case models.MessageTypeOffer, models.MessageTypeAnswer, models.MessageTypeICE:
		return cm.handleWebRTCMessage(conn, &msg)
	default:
		// Broadcast message to room
		return cm.broadcastToRoom(conn.RoomID, &msg, conn.ID)
	}
}

// handleJoinRoom processes room join requests
func (cm *ConnectionManager) handleJoinRoom(conn *WebSocketConnection, msg *models.WebSocketMessage) error {
	// Notify other users in the room
	joinMsg := models.NewWebSocketMessage(
		models.MessageTypeUserJoined,
		conn.RoomID,
		conn.UserID,
		map[string]interface{}{
			"user_id":   conn.UserID,
			"platform":  conn.Platform,
			"device_id": conn.DeviceID,
		},
	)

	return cm.broadcastToRoom(conn.RoomID, joinMsg, conn.ID)
}

// handleLeaveRoom processes room leave requests
func (cm *ConnectionManager) handleLeaveRoom(conn *WebSocketConnection, msg *models.WebSocketMessage) error {
	// Notify other users in the room
	leaveMsg := models.NewWebSocketMessage(
		models.MessageTypeUserLeft,
		conn.RoomID,
		conn.UserID,
		map[string]interface{}{
			"user_id":  conn.UserID,
			"platform": conn.Platform,
		},
	)

	return cm.broadcastToRoom(conn.RoomID, leaveMsg, conn.ID)
}

// handleWebRTCMessage processes WebRTC signaling messages
func (cm *ConnectionManager) handleWebRTCMessage(conn *WebSocketConnection, msg *models.WebSocketMessage) error {
	// WebRTC messages should have a target user
	if msg.TargetID == "" {
		return fmt.Errorf("WebRTC message missing target user ID")
	}

	// Find target connection in the same room
	if roomConns, ok := cm.roomConnections.Load(conn.RoomID); ok {
		roomMap := roomConns.(*sync.Map)

		var targetConn *WebSocketConnection
		roomMap.Range(func(key, value interface{}) bool {
			c := value.(*WebSocketConnection)
			if c.UserID == msg.TargetID {
				targetConn = c
				return false // Stop iteration
			}
			return true
		})

		if targetConn != nil {
			return cm.sendToConnection(targetConn, msg)
		}
	}

	return fmt.Errorf("target user not found in room: %s", msg.TargetID)
}

// handlePing processes ping messages
func (cm *ConnectionManager) handlePing(conn *WebSocketConnection, data []byte) error {
	conn.LastPing = time.Now()

	// Send pong response
	conn.writeMutex.Lock()
	defer conn.writeMutex.Unlock()

	if err := conn.Conn.SetWriteDeadline(time.Now().Add(time.Second * 10)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}

	return wsutil.WriteServerMessage(conn.Conn, ws.OpPong, data)
}

// sendPing sends a ping message to the connection
func (cm *ConnectionManager) sendPing(conn *WebSocketConnection) error {
	if atomic.LoadInt32(&conn.IsActive) == 0 {
		return fmt.Errorf("connection is not active")
	}

	conn.writeMutex.Lock()
	defer conn.writeMutex.Unlock()

	if err := conn.Conn.SetWriteDeadline(time.Now().Add(time.Second * 10)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}

	return wsutil.WriteServerMessage(conn.Conn, ws.OpPing, nil)
}

// writeMessage writes a message to the WebSocket connection
func (cm *ConnectionManager) writeMessage(conn *WebSocketConnection, data []byte) error {
	if atomic.LoadInt32(&conn.IsActive) == 0 {
		return fmt.Errorf("connection is not active")
	}

	conn.writeMutex.Lock()
	defer conn.writeMutex.Unlock()

	if err := conn.Conn.SetWriteDeadline(time.Now().Add(time.Second * 10)); err != nil {
		return fmt.Errorf("failed to set write deadline: %w", err)
	}

	return wsutil.WriteServerMessage(conn.Conn, ws.OpText, data)
}

// sendToConnection sends a message to a specific connection
func (cm *ConnectionManager) sendToConnection(conn *WebSocketConnection, msg *models.WebSocketMessage) error {
	if atomic.LoadInt32(&conn.IsActive) == 0 {
		return fmt.Errorf("connection is not active")
	}

	data, err := msg.ToJSON()
	if err != nil {
		return fmt.Errorf("failed to serialize message: %w", err)
	}

	select {
	case conn.SendChan <- data:
		return nil
	case <-time.After(time.Second * 5):
		return fmt.Errorf("send channel timeout")
	case <-conn.ctx.Done():
		return fmt.Errorf("connection closed")
	}
}

// broadcastToRoom broadcasts a message to all connections in a room
func (cm *ConnectionManager) broadcastToRoom(roomID string, msg *models.WebSocketMessage, excludeConnID string) error {
	if roomID == "" {
		return fmt.Errorf("room ID cannot be empty")
	}
	if msg == nil {
		return fmt.Errorf("message cannot be nil")
	}

	// Publish to Redis for cross-server broadcasting
	channel := fmt.Sprintf("room:%s:messages", roomID)
	if err := cm.redisService.PublishMessage(channel, msg); err != nil {
		utils.Error("Failed to publish message to Redis",
			zap.Error(err),
			zap.String("room_id", roomID),
			zap.String("message_id", msg.MessageID))
	}

	// Also broadcast locally
	return cm.broadcastToRoomLocal(roomID, msg, excludeConnID)
}

// broadcastToRoomLocal broadcasts a message to local connections in a room
func (cm *ConnectionManager) broadcastToRoomLocal(roomID string, msg *models.WebSocketMessage, excludeConnID string) error {
	roomConns, ok := cm.roomConnections.Load(roomID)
	if !ok {
		return nil // No connections in room
	}

	roomMap := roomConns.(*sync.Map)
	var errors []error

	roomMap.Range(func(key, value interface{}) bool {
		conn := value.(*WebSocketConnection)

		// Skip excluded connection
		if conn.ID == excludeConnID {
			return true
		}

		if err := cm.sendToConnection(conn, msg); err != nil {
			errors = append(errors, fmt.Errorf("failed to send to connection %s: %w", conn.ID, err))
		}

		return true
	})

	if len(errors) > 0 {
		utils.Error("Some broadcasts failed",
			zap.String("room_id", roomID),
			zap.Int("error_count", len(errors)))
	}

	return nil
}

// setupRedisSubscriptions sets up Redis pub/sub subscriptions
func (cm *ConnectionManager) setupRedisSubscriptions() error {
	// Subscribe to room message broadcasts
	return cm.redisService.SubscribeToChannel("room:*:messages", cm.handleRedisMessage)
}

// handleRedisMessage handles messages received from Redis pub/sub
func (cm *ConnectionManager) handleRedisMessage(channel string, msg *models.WebSocketMessage) error {
	// Extract room ID from channel name
	// Channel format: "room:{roomID}:messages"
	if len(channel) < 14 { // "room::messages" minimum length
		return fmt.Errorf("invalid channel format: %s", channel)
	}

	roomID := channel[5 : len(channel)-9] // Extract roomID

	// Broadcast to local connections only (avoid Redis loop)
	return cm.broadcastToRoomLocal(roomID, msg, "")
}

// validateMessage validates a WebSocket message
func (cm *ConnectionManager) validateMessage(msg *models.WebSocketMessage) error {
	if msg == nil {
		return fmt.Errorf("message cannot be nil")
	}
	if msg.Type == "" {
		return fmt.Errorf("message type cannot be empty")
	}
	if msg.MessageID == "" {
		msg.MessageID = uuid.New().String()
		utils.Debug("Auto-generated message ID",
			zap.String("message_id", msg.MessageID),
			zap.String("type", string(msg.Type)))
	}

	// Auto-set timestamp if missing
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	return nil
}

// startBackgroundTasks starts background maintenance tasks
func (cm *ConnectionManager) startBackgroundTasks() {
	// Connection cleanup task
	go cm.connectionCleanupTask()

	// Metrics reporting task
	go cm.metricsReportingTask()
}

// connectionCleanupTask periodically cleans up stale connections
func (cm *ConnectionManager) connectionCleanupTask() {
	ticker := time.NewTicker(time.Minute * 5) // Run every 5 minutes
	defer ticker.Stop()

	for {
		select {
		case <-cm.ctx.Done():
			return
		case <-ticker.C:
			cm.cleanupStaleConnections()
		}
	}
}

// cleanupStaleConnections removes connections that haven't pinged recently
func (cm *ConnectionManager) cleanupStaleConnections() {
	staleThreshold := time.Now().Add(-time.Minute * 10) // 10 minutes
	var staleConnections []string

	cm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*WebSocketConnection)
		if conn.LastPing.Before(staleThreshold) {
			staleConnections = append(staleConnections, conn.ID)
		}
		return true
	})

	for _, connID := range staleConnections {
		if err := cm.RemoveConnection(connID); err != nil {
			utils.Error("Failed to remove stale connection",
				zap.Error(err),
				zap.String("connection_id", connID))
		}
	}

	if len(staleConnections) > 0 {
		utils.Info("Cleaned up stale connections",
			zap.Int("count", len(staleConnections)))
	}
}

// metricsReportingTask periodically reports connection metrics
func (cm *ConnectionManager) metricsReportingTask() {
	ticker := time.NewTicker(time.Minute) // Report every minute
	defer ticker.Stop()

	for {
		select {
		case <-cm.ctx.Done():
			return
		case <-ticker.C:
			count := atomic.LoadInt64(&cm.connectionCount)
			utils.Info("Connection metrics",
				zap.Int64("active_connections", count),
				zap.Int64("max_connections", cm.maxConnections))
		}
	}
}

// GetConnectionCount returns the current number of active connections
func (cm *ConnectionManager) GetConnectionCount() int64 {
	return atomic.LoadInt64(&cm.connectionCount)
}

// GetRoomConnectionCount returns the number of connections in a specific room
func (cm *ConnectionManager) GetRoomConnectionCount(roomID string) int {
	if roomConns, ok := cm.roomConnections.Load(roomID); ok {
		roomMap := roomConns.(*sync.Map)
		count := 0
		roomMap.Range(func(key, value interface{}) bool {
			count++
			return true
		})
		return count
	}
	return 0
}

// Close gracefully shuts down the connection manager
func (cm *ConnectionManager) Close() error {
	utils.Info("Shutting down connection manager...")

	// Cancel context to stop all goroutines
	cm.cancel()

	// Close all connections
	cm.connections.Range(func(key, value interface{}) bool {
		conn := value.(*WebSocketConnection)
		cm.RemoveConnection(conn.ID)
		return true
	})

	// Wait for all goroutines to finish
	done := make(chan struct{})
	go func() {
		cm.shutdownWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		utils.Info("Connection manager shut down successfully")
	case <-time.After(cm.config.Server.ShutdownTimeout):
		utils.Warn("Connection manager shutdown timeout exceeded")
	}

	return nil
}

// HealthCheck performs a health check on the connection manager
func (cm *ConnectionManager) HealthCheck() error {
	count := atomic.LoadInt64(&cm.connectionCount)
	if count > cm.maxConnections {
		return fmt.Errorf("connection count exceeds maximum: %d > %d", count, cm.maxConnections)
	}
	return nil
}

// Helper functions
func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}

	// Check for common connection closed errors
	errStr := err.Error()
	return errStr == "EOF" ||
		errStr == "connection reset by peer" ||
		errStr == "broken pipe" ||
		errStr == "use of closed network connection"
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
