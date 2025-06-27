package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"fire-tv-rooms-ecs/internal/config"
	"fire-tv-rooms-ecs/internal/models"
	"fire-tv-rooms-ecs/internal/utils"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type RedisService struct {
	client      *redis.Client
	config      *config.RedisConfig
	subscribers map[string]*redis.PubSub
	subMutex    sync.RWMutex
	ctx         context.Context
	cancel      context.CancelFunc
}

type MessageHandler func(channel string, message *models.WebSocketMessage) error

// NewRedisService creates a new Redis service with connection validation
func NewRedisService(cfg *config.RedisConfig) (*RedisService, error) {
	if cfg == nil {
		return nil, fmt.Errorf("redis config cannot be nil")
	}

	if err := validateRedisConfig(cfg); err != nil {
		return nil, fmt.Errorf("invalid redis config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Create Redis client with optimized settings
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdleConns,
		MaxRetries:   3,
		DialTimeout:  time.Second * 5,
		ReadTimeout:  time.Second * 3,
		WriteTimeout: time.Second * 3,
		PoolTimeout:  time.Second * 4,
	})

	service := &RedisService{
		client:      rdb,
		config:      cfg,
		subscribers: make(map[string]*redis.PubSub),
		ctx:         ctx,
		cancel:      cancel,
	}

	// Test connection
	if err := service.ping(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	utils.Info("Redis service initialized successfully",
		zap.String("addr", cfg.Addr),
		zap.Int("pool_size", cfg.PoolSize),
		zap.Int("min_idle_conns", cfg.MinIdleConns))

	return service, nil
}

// validateRedisConfig validates Redis configuration
func validateRedisConfig(cfg *config.RedisConfig) error {
	if cfg.Addr == "" {
		return fmt.Errorf("redis address cannot be empty")
	}
	if cfg.PoolSize <= 0 {
		return fmt.Errorf("pool size must be greater than 0")
	}
	if cfg.MinIdleConns < 0 {
		return fmt.Errorf("min idle connections cannot be negative")
	}
	if cfg.MinIdleConns > cfg.PoolSize {
		return fmt.Errorf("min idle connections cannot exceed pool size")
	}
	return nil
}

// ping tests Redis connectivity
func (r *RedisService) ping() error {
	ctx, cancel := context.WithTimeout(r.ctx, time.Second*5)
	defer cancel()

	result := r.client.Ping(ctx)
	if result.Err() != nil {
		return fmt.Errorf("redis ping failed: %w", result.Err())
	}

	utils.Debug("Redis ping successful", zap.String("response", result.Val()))
	return nil
}

// PublishMessage publishes a message to a Redis channel
func (r *RedisService) PublishMessage(channel string, message *models.WebSocketMessage) error {
	if channel == "" {
		return fmt.Errorf("channel cannot be empty")
	}
	if message == nil {
		return fmt.Errorf("message cannot be nil")
	}

	// Validate message
	if err := validateWebSocketMessage(message); err != nil {
		return fmt.Errorf("invalid message: %w", err)
	}

	// Create Redis message wrapper
	redisMsg := &models.RedisMessage{
		Channel:   channel,
		Message:   *message,
		Timestamp: time.Now(),
	}

	// Serialize message
	data, err := json.Marshal(redisMsg)
	if err != nil {
		utils.Error("Failed to marshal Redis message",
			zap.Error(err),
			zap.String("channel", channel),
			zap.String("message_id", message.MessageID))
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Publish with timeout
	ctx, cancel := context.WithTimeout(r.ctx, time.Second*3)
	defer cancel()

	result := r.client.Publish(ctx, channel, data)
	if result.Err() != nil {
		utils.Error("Failed to publish message to Redis",
			zap.Error(result.Err()),
			zap.String("channel", channel),
			zap.String("message_id", message.MessageID))
		return fmt.Errorf("failed to publish message: %w", result.Err())
	}

	utils.Debug("Message published successfully",
		zap.String("channel", channel),
		zap.String("message_id", message.MessageID),
		zap.String("message_type", string(message.Type)),
		zap.Int64("subscribers", result.Val()))

	return nil
}

// SubscribeToChannel subscribes to a Redis channel with message handler
func (r *RedisService) SubscribeToChannel(channel string, handler MessageHandler) error {
	if channel == "" {
		return fmt.Errorf("channel cannot be empty")
	}
	if handler == nil {
		return fmt.Errorf("message handler cannot be nil")
	}

	r.subMutex.Lock()
	defer r.subMutex.Unlock()

	// Check if already subscribed
	if _, exists := r.subscribers[channel]; exists {
		return fmt.Errorf("already subscribed to channel: %s", channel)
	}

	// Create subscription
	pubsub := r.client.Subscribe(r.ctx, channel)

	// Test subscription
	_, err := pubsub.Receive(r.ctx)
	if err != nil {
		pubsub.Close()
		return fmt.Errorf("failed to subscribe to channel %s: %w", channel, err)
	}

	r.subscribers[channel] = pubsub

	utils.Info("Subscribed to Redis channel",
		zap.String("channel", channel))

	// Start message processing goroutine
	go r.processMessages(channel, pubsub, handler)

	return nil
}

// processMessages processes incoming messages from Redis subscription
func (r *RedisService) processMessages(channel string, pubsub *redis.PubSub, handler MessageHandler) {
	defer func() {
		if err := recover(); err != nil {
			utils.Error("Panic in message processing goroutine",
				zap.String("channel", channel),
				zap.Any("error", err))
		}
	}()

	ch := pubsub.Channel()

	for {
		select {
		case <-r.ctx.Done():
			utils.Info("Stopping message processing due to context cancellation",
				zap.String("channel", channel))
			return

		case msg, ok := <-ch:
			if !ok {
				utils.Warn("Redis subscription channel closed",
					zap.String("channel", channel))
				return
			}

			if err := r.handleRedisMessage(channel, msg, handler); err != nil {
				utils.Error("Failed to handle Redis message",
					zap.Error(err),
					zap.String("channel", channel))
			}
		}
	}
}

// handleRedisMessage processes a single Redis message
func (r *RedisService) handleRedisMessage(channel string, msg *redis.Message, handler MessageHandler) error {
	if msg == nil {
		return fmt.Errorf("received nil message")
	}

	// Parse Redis message
	var redisMsg models.RedisMessage
	if err := json.Unmarshal([]byte(msg.Payload), &redisMsg); err != nil {
		utils.Error("Failed to unmarshal Redis message",
			zap.Error(err),
			zap.String("channel", channel),
			zap.String("payload", msg.Payload))
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}

	// Validate parsed message
	if err := validateWebSocketMessage(&redisMsg.Message); err != nil {
		utils.Error("Received invalid WebSocket message from Redis",
			zap.Error(err),
			zap.String("channel", channel))
		return fmt.Errorf("invalid websocket message: %w", err)
	}

	// Call handler
	if err := handler(channel, &redisMsg.Message); err != nil {
		utils.Error("Message handler failed",
			zap.Error(err),
			zap.String("channel", channel),
			zap.String("message_id", redisMsg.Message.MessageID))
		return fmt.Errorf("handler failed: %w", err)
	}

	utils.Debug("Message processed successfully",
		zap.String("channel", channel),
		zap.String("message_id", redisMsg.Message.MessageID),
		zap.String("message_type", string(redisMsg.Message.Type)))

	return nil
}

// UnsubscribeFromChannel unsubscribes from a Redis channel
func (r *RedisService) UnsubscribeFromChannel(channel string) error {
	if channel == "" {
		return fmt.Errorf("channel cannot be empty")
	}

	r.subMutex.Lock()
	defer r.subMutex.Unlock()

	pubsub, exists := r.subscribers[channel]
	if !exists {
		return fmt.Errorf("not subscribed to channel: %s", channel)
	}

	// Close subscription
	if err := pubsub.Close(); err != nil {
		utils.Error("Failed to close Redis subscription",
			zap.Error(err),
			zap.String("channel", channel))
		return fmt.Errorf("failed to close subscription: %w", err)
	}

	delete(r.subscribers, channel)

	utils.Info("Unsubscribed from Redis channel",
		zap.String("channel", channel))

	return nil
}

// SetConnectionState stores connection state in Redis
func (r *RedisService) SetConnectionState(connID string, conn *models.Connection) error {
	if connID == "" {
		return fmt.Errorf("connection ID cannot be empty")
	}
	if conn == nil {
		return fmt.Errorf("connection cannot be nil")
	}

	if err := validateConnection(conn); err != nil {
		return fmt.Errorf("invalid connection: %w", err)
	}

	data, err := json.Marshal(conn)
	if err != nil {
		return fmt.Errorf("failed to marshal connection: %w", err)
	}

	ctx, cancel := context.WithTimeout(r.ctx, time.Second*3)
	defer cancel()

	key := fmt.Sprintf("conn:%s", connID)
	result := r.client.Set(ctx, key, data, time.Hour*24) // 24 hour TTL

	if result.Err() != nil {
		utils.Error("Failed to store connection state",
			zap.Error(result.Err()),
			zap.String("connection_id", connID))
		return fmt.Errorf("failed to store connection state: %w", result.Err())
	}

	utils.Debug("Connection state stored",
		zap.String("connection_id", connID),
		zap.String("user_id", conn.UserID),
		zap.String("room_id", conn.RoomID))

	return nil
}

// GetConnectionState retrieves connection state from Redis
func (r *RedisService) GetConnectionState(connID string) (*models.Connection, error) {
	if connID == "" {
		return nil, fmt.Errorf("connection ID cannot be empty")
	}

	ctx, cancel := context.WithTimeout(r.ctx, time.Second*3)
	defer cancel()

	key := fmt.Sprintf("conn:%s", connID)
	result := r.client.Get(ctx, key)

	if result.Err() != nil {
		if result.Err() == redis.Nil {
			return nil, fmt.Errorf("connection not found: %s", connID)
		}
		return nil, fmt.Errorf("failed to get connection state: %w", result.Err())
	}

	var conn models.Connection
	if err := json.Unmarshal([]byte(result.Val()), &conn); err != nil {
		return nil, fmt.Errorf("failed to unmarshal connection: %w", err)
	}

	return &conn, nil
}

// DeleteConnectionState removes connection state from Redis
func (r *RedisService) DeleteConnectionState(connID string) error {
	if connID == "" {
		return fmt.Errorf("connection ID cannot be empty")
	}

	ctx, cancel := context.WithTimeout(r.ctx, time.Second*3)
	defer cancel()

	key := fmt.Sprintf("conn:%s", connID)
	result := r.client.Del(ctx, key)

	if result.Err() != nil {
		return fmt.Errorf("failed to delete connection state: %w", result.Err())
	}

	utils.Debug("Connection state deleted",
		zap.String("connection_id", connID))

	return nil
}

// GetRoomConnections gets all connections for a room
func (r *RedisService) GetRoomConnections(roomID string) ([]*models.Connection, error) {
	if roomID == "" {
		return nil, fmt.Errorf("room ID cannot be empty")
	}

	ctx, cancel := context.WithTimeout(r.ctx, time.Second*5)
	defer cancel()

	pattern := "conn:*"
	keys := r.client.Keys(ctx, pattern)

	if keys.Err() != nil {
		return nil, fmt.Errorf("failed to get connection keys: %w", keys.Err())
	}

	var connections []*models.Connection

	for _, key := range keys.Val() {
		result := r.client.Get(ctx, key)
		if result.Err() != nil {
			continue // Skip failed keys
		}

		var conn models.Connection
		if err := json.Unmarshal([]byte(result.Val()), &conn); err != nil {
			continue // Skip invalid connections
		}

		if conn.RoomID == roomID {
			connections = append(connections, &conn)
		}
	}

	return connections, nil
}

// Close gracefully closes the Redis service
func (r *RedisService) Close() error {
	utils.Info("Closing Redis service...")

	// Cancel context to stop all goroutines
	r.cancel()

	// Close all subscriptions
	r.subMutex.Lock()
	for channel, pubsub := range r.subscribers {
		if err := pubsub.Close(); err != nil {
			utils.Error("Failed to close subscription",
				zap.Error(err),
				zap.String("channel", channel))
		}
	}
	r.subMutex.Unlock()

	// Close Redis client
	if err := r.client.Close(); err != nil {
		utils.Error("Failed to close Redis client", zap.Error(err))
		return fmt.Errorf("failed to close redis client: %w", err)
	}

	utils.Info("Redis service closed successfully")
	return nil
}

// Health check for Redis service
func (r *RedisService) HealthCheck() error {
	return r.ping()
}

// validateWebSocketMessage validates a WebSocket message
func validateWebSocketMessage(msg *models.WebSocketMessage) error {
	if msg == nil {
		return fmt.Errorf("message cannot be nil")
	}
	if msg.Type == "" {
		return fmt.Errorf("message type cannot be empty")
	}
	if msg.MessageID == "" {
		return fmt.Errorf("message ID cannot be empty")
	}
	if msg.Timestamp.IsZero() {
		return fmt.Errorf("timestamp cannot be zero")
	}
	return nil
}

// validateConnection validates a connection object
func validateConnection(conn *models.Connection) error {
	if conn == nil {
		return fmt.Errorf("connection cannot be nil")
	}
	if conn.ID == "" {
		return fmt.Errorf("connection ID cannot be empty")
	}
	if conn.UserID == "" {
		return fmt.Errorf("user ID cannot be empty")
	}
	if conn.RoomID == "" {
		return fmt.Errorf("room ID cannot be empty")
	}
	if conn.Platform == "" {
		return fmt.Errorf("platform cannot be empty")
	}
	return nil
}
